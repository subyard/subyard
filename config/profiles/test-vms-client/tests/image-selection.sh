#!/usr/bin/env bash
# Image choice is bounded and preserves old requests when omitted.
set -euo pipefail
unset SUBYARD_E2E_LEASE_EVENTS SUBYARD_E2E_LEASE_NONCE
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=config/profiles/test-vms-client/skills/subyard-test-vms/client.sh
. "$ROOT/config/profiles/test-vms-client/skills/subyard-test-vms/client.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
LEASE_REQUESTED_SLOT=slot-001 LEASE_YARD=yard LEASE_PROJECT=project LEASE_RUN=run LEASE_PURPOSE=tests
base='acquire-v3 subyard-pair client fingerprint yard project run tests ssh-ed25519 blob slot-001'
[ "$(lease_acquire_request client fingerprint ssh-ed25519 blob)" = "$base" ] || fail 'legacy request changed'
for image in debian-13 ubuntu-24.04 ubuntu-26.04; do
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
for image in images:ubuntu/24.04/cloud ubuntu-25.04 ''; do
 if (allocation_option --image "$image") >/dev/null 2>&1; then fail 'unallowlisted image accepted'; fi
done
IMAGE_REQUESTED=debian-13
if (allocation_option --image ubuntu-24.04) >/dev/null 2>&1; then fail 'duplicate image accepted'; fi
printf 'ok: image allowlist and compatible lease request forwarding\n'

fixture="$(mktemp -d /tmp/subyard-image-selection.XXXXXX)"
trap 'rm -rf -- "$fixture"' EXIT

# Exercise real capability negotiation and grant validation with a fake facade.
for scenario in unsupported legacy wrong-image; do
  mkdir -p "$fixture/$scenario"
  rc=0
  (
    LOCAL_TEMP="$fixture/$scenario"
    IMAGE_REQUESTED='' VM_COUNT_REQUESTED=1 LEASE_RUN=1234abcd
    LEASE_REQUESTED_SLOT='slot-001'
    export SUBYARD_E2E_LEASE_IMAGE=debian-13
    [ "$scenario" = legacy ] || SUBYARD_E2E_LEASE_IMAGE=ubuntu-26.04
    ensure_identity() { IDENTITY="$LOCAL_TEMP/lease_id_ed25519"; }
    resolve_bastion_route() { :; }
    ensure_client_id() { printf client; }
    lease_attribution() { printf 'yard\tproject\n'; }
    telemetry_correlation() { :; }
    write_client_config() { touch "$LOCAL_TEMP/config-written"; }
    facade_request() {
      printf '%s\n' "$1" >> "$LOCAL_TEMP/requests"
      if [ "$1" = status ]; then
        capabilities='["attribution-v2","environment-acquire-v3","disposable-v1","vm-count-v1"]'
        [ "$scenario" != wrong-image ] || capabilities="$(jq '. + ["image-v1"]' <<<"$capabilities")"
        jq -cn --argjson capabilities "$capabilities" '{schema_version:1,status:"ok",capabilities:$capabilities}'
      else
        # An old broker omits image; a buggy image-aware broker returns Debian.
        jq -cn --arg scenario "$scenario" '{schema_version:1,status:"ok",grant:{
          environment:({type:"subyard-pair",vm_count:1,cpu_per_vm:2,memory_per_vm:"4GiB",
            disk_per_vm:"20GiB",lifecycle:"disposable-v1"} +
            (if $scenario == "wrong-image" then {image:"debian-13"} else {} end)),
          base_fingerprint:("a" * 64),slot_id:"slot-001",resource_generation:1,
          lease_id:"aabb",lease_epoch:1,capability:"ccdd",data_user:"subyard-e2e-slot-1",
          context:{schema_version:2,yard:"yard",project:"project",run:"1234abcd",purpose:"tests"},
          targets:[{selector:1,name:"e2e-vm-1",address:"10.42.1.11",
            host_key_type:"ssh-ed25519",host_key_blob:"YWJjZA=="}]}}'
      fi
    }
    acquire_lease
  ) >"$fixture/$scenario/output" 2>&1 || rc=$?
  case "$scenario" in
    unsupported)
      [ "$rc" != 0 ] || fail 'old broker accepted non-default image'
      [ "$(cat "$fixture/$scenario/requests")" = status ] || fail 'unsupported image reached acquisition'
      rg -q 'explicit images \(image-v1\)' "$fixture/$scenario/output" || fail 'missing capability diagnostic'
      ;;
    legacy)
      [ "$rc" = 0 ] || fail 'default image failed with old broker'
      rg -q '^acquire-v3 .* slot-001 1$' "$fixture/$scenario/requests" || fail 'Debian fallback changed the legacy wire format'
      [ -f "$fixture/$scenario/config-written" ] || fail 'legacy grant did not reach guest configuration'
      ;;
    wrong-image)
      [ "$rc" != 0 ] || fail 'wrong OS image accepted'
      rg -q '^acquire-v3 .* 1 - ubuntu-26[.]04$' "$fixture/$scenario/requests" || fail 'wrong-image test never acquired'
      rg -q 'invalid disposable environment grant' "$fixture/$scenario/output" || fail 'missing wrong-image refusal'
      [ ! -f "$fixture/$scenario/config-written" ] || fail 'wrong image reached guest configuration'
      ;;
  esac
done
printf 'ok: image capability negotiation, legacy broker compatibility and wrong-image refusal\n'

# Exercise the NM controller's real option parser without acquiring a VM.
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
for image in default debian-13 ubuntu-24.04 ubuntu-26.04; do
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
