#!/usr/bin/env bash
# Two RTC sleeps and a real awake owner share one fresh disposable pair.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
native=''
slot_seen=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] && [ "$slot_seen" = 0 ] || die 'expected one --slot N'
      set_requested_slot "$2" --slot; slot_seen=1; shift 2 ;;
    --native)
      [ "$#" -ge 2 ] && [ -z "$native" ] || die 'expected one --native relative-artifact'
      native="$2"; shift 2 ;;
    -h|--help)
      printf 'Usage: dev/e2e/veranda-sleep-acceptance.sh --slot N --native .build/ARTIFACT\n'
      exit 0 ;;
    *) die 'unknown argument' ;;
  esac
done
[ "$slot_seen" = 1 ] || die '--slot N is required'
[[ "$native" =~ ^\.build/[a-zA-Z0-9._-]+$ ]] || die '--native must name a frozen native test artifact'
LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
chmod 0700 "$LOCAL_TEMP"
sleep_token="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
# shellcheck disable=SC2034
LEASE_PURPOSE=veranda-sleep
owner_setup=0 owner_ready=0 client_started=0

payload() {
  local vm="$1"; shift
  guest "$vm" env SUBYARD_E2E_RUN_ID="$LEASE_RUN" SUBYARD_E2E_SLOT="$LEASE_SLOT" \
    SUBYARD_E2E_VM="$vm" SUBYARD_E2E_TYPE=subyard-pair SUBYARD_E2E_PURPOSE="$LEASE_PURPOSE" \
    SUBYARD_E2E_SLEEP_TOKEN="$sleep_token" \
    /usr/bin/python3 "${GUEST_DIRS[$vm]}/src/dev/e2e/veranda-sleep.py" "$@"
}
preview() {
  local phase="$1"
  guest 2 bash -c '
    log="$1/$2.log"; : > "$log"; chmod 0600 "$log"
    exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev \
      SUBYARD_E2E_RUN_ID="$3" SUBYARD_E2E_VM=2 SUBYARD_E2E_TYPE=subyard-pair \
      bash "$1/src/dev/e2e/preview-lifecycle.sh" "$2" > "$log" 2>&1
  ' subyard "${GUEST_DIRS[2]}" "$phase" "$LEASE_RUN"
}
cleanup_acceptance() {
  local original=$? failed=0 vm rc
  trap - EXIT INT TERM
  set +e
  phase_end "$original"
  for vm in 1 2; do
    [ -n "${GUEST_DIRS[$vm]:-}" ] || continue
    phase_start guest-cleanup "$vm"
    rc=0
    if { [ "$vm" = 1 ] && [ "$client_started" = 1 ]; } || { [ "$vm" = 2 ] && [ "$owner_ready" = 1 ]; }; then
      payload "$vm" --cleanup || rc=1
    fi
    if [ "$vm" = 2 ] && [ "$owner_setup" = 1 ]; then
      preview owner-cleanup || rc=1
    fi
    cleanup_guest "$vm" || rc=1
    phase_end "$rc"
    [ "$rc" = 0 ] || failed=1
  done
  phase_start cleanup/release
  if [ -n "${LEASE_KEEPER_PID:-}" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
  fi
  release_lease || failed=1
  case "$LOCAL_TEMP" in /tmp/subyard-agent-e2e.*|"${TMPDIR:-/tmp}"/subyard-agent-e2e.*)
    find "$LOCAL_TEMP" -depth -delete || failed=1 ;; *) failed=1 ;;
  esac
  phase_end "$failed"
  printf 'sleep-controller: original_exit_code=%s cleanup_failed=%s\n' "$original" "$failed"
  [ "$original" != 0 ] || { [ "$failed" = 0 ] || original=3; }
  exit "$original"
}
trap cleanup_acceptance EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

phase_start allocation
acquire_lease
phase_end 0
start_lease_keeper
phase_start packing
bundle="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
phase_end 0
for vm in 1 2; do
  phase_start transport "$vm"
  prepare_guest "$vm" "$bundle" "$bundle_hash"
  if [ "$vm" = 2 ]; then guest "$vm" chown -R dev:dev "${GUEST_DIRS[$vm]}"; fi
  phase_end 0
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
printf '%s\n' "${GUEST_DIRS[2]}/src" > "$LOCAL_TEMP/peer-source"
chmod 0600 "$LOCAL_TEMP/peer-source"
guest 1 dd "of=$guest_root/peer-source" status=none < "$LOCAL_TEMP/peer-source"
guest 1 chmod 0600 "$guest_root/peer-key" "$guest_root/peer-known-hosts" "$guest_root/peer-config" "$guest_root/peer-source"
phase_start guest 2
# The controller freeze supplies the current engine as .build/yard; no guest build.
guest 2 test -x "${GUEST_DIRS[2]}/src/.build/yard"
preview owner-setup
owner_setup=1
payload 2 --owner-start
owner_ready=1
phase_end 0
phase_start guest 1
client_started=1
payload 1 --acceptance-client --native "$native" --peer-config "$guest_root/peer-config" \
  --destination "root@${VM_IP[2]}:22"
phase_end 0
