#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home"
export PATH="$TMP/bin:$PATH"
export MOCK_INCUS_LOG="$TMP/incus.log"
export MOCK_PROJECT="$TMP/project"
export MOCK_RAW_EFFECTIVE=''
export MOCK_RAW_LOCAL=''
export MOCK_PROFILE_RAW=''
export MOCK_GUEST_RULE_INPUT="$TMP/page-reporting-rule"
mkdir -p "$HOME" "$TMP/bin"

cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s|%s\n' "${INCUS_PROJECT:-}" "$*" >>"$MOCK_INCUS_LOG"
case "${1:-} ${2:-}" in
  'info ') exit 0 ;;
  'project show') [ -e "$MOCK_PROJECT" ] ;;
  'project create')
    [ "${INCUS_PROJECT:-}" = default ]
    if IFS= read -r unexpected; then
      printf 'project create consumed stdin: %s\n' "$unexpected" >&2
      exit 91
    fi
    touch "$MOCK_PROJECT"
    ;;
  'project set' | 'project unset') ;;
  'project get') ;;
  'profile get')
    [ "${4:-}" != raw.qemu.conf ] || printf '%s\n' "$MOCK_PROFILE_RAW"
    ;;
  'config get')
    if [ "${4:-}" = raw.qemu.conf ]; then
      case "${5:-}" in
        --expanded) printf '%s\n' "$MOCK_RAW_EFFECTIVE" ;;
        --project) printf '%s\n' "$MOCK_RAW_LOCAL" ;;
        *) exit 90 ;;
      esac
    fi
    ;;
  'exec '*)
    [ "${MOCK_GUEST_EXEC_FAIL:-0}" = 0 ] || exit 1
    case "$*" in
      *'count=0'*) printf '%s\n' "${MOCK_GUEST_BITS:-000001}" ;;
      *'systemd-tmpfiles --create'*) cat > "$MOCK_GUEST_RULE_INPUT" ;;
      *'expected=$1'*) printf '%s\n' "${MOCK_GUEST_STATE:-ready}" ;;
      *) exit 90 ;;
    esac
    ;;
  'profile device')
    [ "${3:-}" = list ] || [ "${3:-}" = add ]
    ;;
  *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK
chmod +x "$TMP/bin/incus"

printf 'must-not-reach-incus\n' | "$ROOT/scripts/02-create-project.sh" --yes >/dev/null
"$ROOT/scripts/02-create-project.sh" --yes >/dev/null

[ "$(grep -Fc 'default|project create subyard' "$MOCK_INCUS_LOG")" = 1 ] \
  || fail 'project creation did not use the default Incus context exactly once'
printf 'ok: project creation closes stdin and is independent of its absent context\n'

cat > "$TMP/bin/qemu-system-x86_64" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  --version) printf 'QEMU emulator version %s\n' "${MOCK_QEMU_VERSION:-8.2.0}" ;;
  '-device virtio-balloon-pci,help') printf '%s\n' "${MOCK_QEMU_PROPERTY:-free-page-reporting=<bool>}" ;;
  *) exit 90 ;;
esac
MOCK
chmod +x "$TMP/bin/qemu-system-x86_64"

: > "$MOCK_INCUS_LOG"
VM_FREE_PAGE_REPORTING=1 YARD_KIND=vm "$ROOT/scripts/02-create-project.sh" --yes >/dev/null
grep -Fxq 'subyard|project set subyard restricted.virtual-machines.lowlevel allow' "$MOCK_INCUS_LOG" \
  || fail 'opted-in VM project did not authorize the fixed VM override'

# shellcheck source=scripts/lib-vm-page-reporting.sh
. "$ROOT/scripts/lib-vm-page-reporting.sh"
vm_page_reporting_check_host || fail 'tested QEMU reporting property was rejected'
MOCK_QEMU_VERSION=10.0.13
export MOCK_QEMU_VERSION
vm_page_reporting_check_host || fail 'Debian 13 QEMU reporting property was rejected'
MOCK_QEMU_VERSION=7.2.0
export MOCK_QEMU_VERSION
if vm_page_reporting_check_host >/dev/null 2>&1; then fail 'unsupported QEMU version accepted'; fi
MOCK_QEMU_VERSION=8.2.0
MOCK_QEMU_PROPERTY=other-property
export MOCK_QEMU_VERSION MOCK_QEMU_PROPERTY
if vm_page_reporting_check_host >/dev/null 2>&1; then fail 'missing QEMU reporting property accepted'; fi

MOCK_RAW_EFFECTIVE="$VM_PAGE_REPORTING_CONF"
MOCK_RAW_LOCAL="$VM_PAGE_REPORTING_CONF"
vm_page_reporting_check_raw subyard-vpn yard-vpn true || fail 'fixed local override rejected'
MOCK_RAW_LOCAL=''
if vm_page_reporting_check_raw subyard-vpn yard-vpn true >/dev/null 2>&1; then
  fail 'inherited raw QEMU override accepted'
fi
MOCK_RAW_EFFECTIVE='-device virtio-balloon-pci'
if vm_page_reporting_check_raw subyard-vpn yard-vpn true >/dev/null 2>&1; then
  fail 'foreign raw QEMU override accepted'
fi
MOCK_PROFILE_RAW="$VM_PAGE_REPORTING_CONF"
if vm_page_reporting_check_profile subyard-vpn >/dev/null 2>&1; then
  fail 'raw QEMU override inherited from project profile was accepted'
fi
MOCK_GUEST_BITS=000001
export MOCK_GUEST_BITS
vm_page_reporting_check_guest subyard-vpn yard-vpn || fail 'negotiated guest reporting bit was rejected'
vm_page_reporting_prepare_guest subyard-vpn yard-vpn || fail 'fixed guest reporting order was not installed'
printf '%s\n' "$VM_PAGE_REPORTING_TMPFILES_RULE" | cmp -s - "$MOCK_GUEST_RULE_INPUT" \
  || fail 'guest reporting order rule differed from fixed content'
MOCK_GUEST_STATE=rule-mode
export MOCK_GUEST_STATE
guest_check_rc=0
vm_page_reporting_check_guest subyard-vpn yard-vpn >/dev/null 2>"$TMP/guest-error" || guest_check_rc=$?
[ "$guest_check_rc" = 10 ] || fail 'guest reporting rule ownership drift did not return 10'
grep -Fq 'ownership or mode drifted' "$TMP/guest-error" \
  || fail 'guest reporting rule ownership drift lacked a precise diagnostic'
MOCK_GUEST_STATE=order-value
guest_check_rc=0
vm_page_reporting_check_guest subyard-vpn yard-vpn >/dev/null 2>"$TMP/guest-error" || guest_check_rc=$?
[ "$guest_check_rc" = 10 ] || fail 'guest reporting order drift did not return 10'
grep -Fq 'page_reporting_order is not 1' "$TMP/guest-error" \
  || fail 'guest reporting order drift lacked a precise diagnostic'
MOCK_GUEST_STATE=ready
MOCK_GUEST_BITS=100000
if vm_page_reporting_check_guest subyard-vpn yard-vpn > /dev/null 2> "$TMP/guest-error"; then
  fail 'guest feature bit 0 was mistaken for negotiated reporting bit 5'
fi
grep -Fq 'feature bit 5 was not negotiated' "$TMP/guest-error" \
  || fail 'missing negotiated bit did not give a precise diagnostic'
MOCK_GUEST_BITS=missing
if vm_page_reporting_check_guest subyard-vpn yard-vpn >/dev/null 2>&1; then
  fail 'missing guest balloon device was accepted'
fi
MOCK_GUEST_BITS=invalid
if vm_page_reporting_check_guest subyard-vpn yard-vpn >/dev/null 2>&1; then
  fail 'malformed guest feature bits were accepted'
fi
MOCK_GUEST_EXEC_FAIL=1
export MOCK_GUEST_EXEC_FAIL
if vm_page_reporting_check_guest subyard-vpn yard-vpn >/dev/null 2>&1; then
  fail 'unavailable guest agent was accepted'
fi
unset MOCK_GUEST_EXEC_FAIL
MOCK_GUEST_BITS=000001
VM_FREE_PAGE_REPORTING=1 YARD_KIND=vm \
  "$ROOT/scripts/03-create-subyard.sh" --check-page-reporting-guest >/dev/null \
  || fail 'read-only guest reporting check rejected the negotiated feature'
MOCK_GUEST_BITS=000000
guest_check_rc=0
VM_FREE_PAGE_REPORTING=1 YARD_KIND=vm \
  "$ROOT/scripts/03-create-subyard.sh" --check-page-reporting-guest >/dev/null 2>&1 \
  || guest_check_rc=$?
[ "$guest_check_rc" = 10 ] || fail 'read-only guest reporting check did not return drift 10'
printf 'ok: owner VM reporting preflight rejects unsupported and inherited raw QEMU settings\n'

# shellcheck source=scripts/lib-vm-storage.sh
. "$ROOT/scripts/lib-vm-storage.sh"
SRV_VOLUME_SIZE=2GiB
[ "$(vm_storage_size_bytes)" = 2147483648 ] || fail 'VM block volume size was not converted exactly'
SRV_VOLUME_SIZE=2GB
if vm_storage_size_bytes >/dev/null 2>&1; then fail 'ambiguous block volume size accepted'; fi
SRV_VOLUME_SIZE=2GiB
YARD_KIND=vm
SRV_VOLUME_TYPE=block
SRV_POOL=default
SRV_VOLUME=yard-srv
vm_storage_volume() { printf '%s\n' '{"content_type":"filesystem","config":{"size":"2GiB","user.subyard.storage":"v1"}}'; }
storage_result=0
( die() { exit 77; }; vm_storage_attach >/dev/null 2>&1 ) || storage_result=$?
[ "$storage_result" = 77 ] || fail 'foreign VM volume was accepted for block attachment'
MOCK_STORAGE_FORMAT=pending
MOCK_MOUNT_UUID=11111111-1111-1111-1111-111111111111
vm_storage_volume() {
  printf '{"content_type":"block","config":{"size":"2GiB","user.subyard.storage":"v1","user.subyard.format":"%s","user.subyard.uuid":"11111111-1111-1111-1111-111111111111"}}\n' "$MOCK_STORAGE_FORMAT"
}
device_get() {
  case "$2" in
    type) printf 'disk\n' ;;
    pool) printf '%s\n' "$SRV_POOL" ;;
    source) printf '%s\n' "$SRV_VOLUME" ;;
    path) printf '\n' ;;
    io.bus) printf 'virtio-scsi\n' ;;
  esac
}
power_state() { printf 'RUNNING\n'; }
incus() { [ "$1" = exec ] && printf '%s\n' "$MOCK_MOUNT_UUID"; }
if vm_storage_check; then fail 'pending VM state volume was considered converged'; fi
MOCK_STORAGE_FORMAT=ready
MOCK_MOUNT_UUID=22222222-2222-2222-2222-222222222222
if vm_storage_check; then fail 'wrong guest /srv mount was considered converged'; fi
MOCK_MOUNT_UUID=11111111-1111-1111-1111-111111111111
vm_storage_check || fail 'ready VM state volume with matching guest mount was rejected'

ROOT_DISK_SIZE=10GiB
ALLOWS_HOST_ACCESS=false
MOCK_PROFILE='{"devices":{"root":{"type":"disk","pool":"default","path":"/","size":"10GiB"},"eth0":{"type":"nic","network":"incusbr0"}}}'
MOCK_INSTANCE='{"devices":{"srv":{"type":"disk","pool":"default","source":"yard-srv","io.bus":"virtio-scsi"}},"expanded_devices":{"root":{"type":"disk","pool":"default","path":"/","size":"10GiB"},"eth0":{"type":"nic","network":"incusbr0"},"srv":{"type":"disk","pool":"default","source":"yard-srv","io.bus":"virtio-scsi"}}}'
incus() {
  [ "$1" = query ] || return 90
  case "$2" in
    /1.0/instances/*) printf '%s\n' "$MOCK_INSTANCE" ;;
    /1.0/profiles/default*) printf '%s\n' "$MOCK_PROFILE" ;;
    *) return 90 ;;
  esac
}
vm_storage_check_device_boundary false || fail 'managed effective VM devices were rejected'
baseline="$MOCK_INSTANCE"
MOCK_INSTANCE="$(jq -c '.devices.eth0=(.expanded_devices.eth0 + {"ipv4.address":"10.0.0.2"}) | .expanded_devices.eth0=.devices.eth0' <<<"$baseline")"
vm_storage_check_device_boundary false || fail 'managed local NIC address was rejected by host access boundary'
MOCK_INSTANCE="$(jq -c '.expanded_devices.eth0.network="foreign"' <<<"$MOCK_INSTANCE")"
if vm_storage_check_device_boundary false >/dev/null 2>&1; then fail 'foreign primary NIC was accepted'; fi
MOCK_INSTANCE="$(jq -c '.expanded_devices["host-cache"]={"type":"disk","source":"/srv/cache","path":"/mnt/cache"}' <<<"$baseline")"
if vm_storage_check_device_boundary false >/dev/null 2>&1; then fail 'inherited host disk was accepted'; fi
MOCK_INSTANCE="$(jq -c '.expanded_devices["host-kvm"]={"type":"unix-char","source":"/dev/kvm","path":"/dev/kvm"}' <<<"$baseline")"
if vm_storage_check_device_boundary false >/dev/null 2>&1; then fail 'inherited host device was accepted'; fi
MOCK_INSTANCE="$(jq -c '.devices.root={"type":"disk","pool":"other","path":"/","size":"20GiB"} | .expanded_devices.root=.devices.root' <<<"$baseline")"
if vm_storage_check_device_boundary false >/dev/null 2>&1; then fail 'local root override was accepted'; fi
MOCK_INSTANCE="$(jq -c '.expanded_devices.root.size="20GiB"' <<<"$baseline")"
if vm_storage_check_device_boundary false >/dev/null 2>&1; then fail 'effective root size drift was accepted'; fi
MOCK_INSTANCE="$(jq -c --arg source "$SUBYARD_HOME/e2e/routes" '
  .devices["subyard-e2e-routes"]={"type":"disk","source":$source,"path":"/var/lib/subyard/e2e-routes","readonly":"true"} |
  .expanded_devices["subyard-e2e-routes"]=.devices["subyard-e2e-routes"]' <<<"$baseline")"
vm_storage_check_device_boundary true || fail 'exact old Subyard route mount cannot be removed safely'
if vm_storage_check_device_boundary false >/dev/null 2>&1; then fail 'old Subyard route mount remained after removal boundary'; fi
MOCK_INSTANCE="$baseline"
printf 'ok: effective VM root and host-device boundary rejects inherited and local overrides\n'
