#!/usr/bin/env bash
# Singleton controller adapter for a fresh ubuntu-24.04 GitHub-hosted job.
# The job lifetime fences access even if its controller is killed.
runner_job() {
  [ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] &&
    [[ "${GITHUB_RUN_ID:-}" =~ ^[0-9]+$ && "${GITHUB_RUN_ATTEMPT:-}" =~ ^[0-9]+$ &&
       "${GITHUB_JOB:-}" =~ ^[a-zA-Z_][a-zA-Z0-9_-]*$ ]] &&
    [ "$(. /etc/os-release && printf '%s-%s' "$ID" "$VERSION_ID")" = ubuntu-24.04 ] &&
    [ -s /imagegeneration/imagedata.json ] || return 1
  printf '%s/%s/%s\n' "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT" "$GITHUB_JOB"
}

prepare_runner_lease() {
  local job
  job="$(runner_job)" || { printf 'runner lease needs a fresh ubuntu-24.04 hosted job\n' >&2; return 2; }
  id dev >/dev/null
  test -x /usr/sbin/sshd
  test -s /etc/ssh/ssh_host_ed25519_key.pub
  sudo -n mkdir -m 0755 /run/subyard-e2e-runner
  printf '%s\n' "$job" | sudo -n install -m 0444 /dev/stdin /run/subyard-e2e-runner/job
  sudo -n install -d -m 0755 /run/sshd
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  [ "$#" = 1 ] && [ "$1" = prepare ] || exit 2
  prepare_runner_lease
  exit
fi

# The selected hosted job supplies the route. Keep native broker diagnostics and
# boundary probes on the pool jobs that actually own a test-vms service.
resolve_service_selection() {
  case "$1" in run|ssh) ;; *) die 'runner leases support guest commands only' ;; esac
  [ -z "$E2E_OWNER$E2E_POOL" ] && [ "$E2E_LEGACY_ROUTE" = 0 ] || die 'runner leases have no broker route selector'
}

# The shared controller consumes the lease and transport globals.
# shellcheck disable=SC2034
acquire_lease() {
  local job attribution facts disk_bytes free_bytes
  job="$(runner_job)" && [ "$(cat /run/subyard-e2e-runner/job)" = "$job" ] ||
    die 'runner lease requires a prepared ubuntu-24.04 hosted job'
  resolve_vm_count
  [ "$LEASE_REQUESTED_SLOT" = slot-001 ] && [ "$VM_COUNT" = 1 ] &&
    [ "$ENVIRONMENT_TYPE" != system-container ] || die 'runner lease requires slot 1 and one host'
  [ -z "$IMAGE_REQUESTED" ] || [ "$IMAGE_REQUESTED" = ubuntu-24.04 ] || die 'runner image must be ubuntu-24.04'
  [ -d "$LOCAL_TEMP" ] && [ -f "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ] &&
    [[ "${SUBYARD_E2E_CANDIDATE_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] || die 'runner lease requires the frozen candidate'
  read -r disk_bytes free_bytes < <(df -B1 --output=size,avail /var/tmp | tail -1)
  [ -z "$DISK_REQUESTED" ] || [ "$free_bytes" -ge "$((50*1024**3))" ] || die 'hosted runner has less than 50 GiB free'
  GRANT_DISK_PER_VM="$((disk_bytes/1024**3))GiB"
  GRANT_MEMORY_PER_VM="$(awk '/^MemTotal:/ {printf "%dGiB", $2/1048576}' /proc/meminfo)"
  GRANT_CPU_PER_VM="$(nproc)"
  attribution="$(lease_attribution)"
  IFS=$'\t' read -r LEASE_YARD LEASE_PROJECT <<<"$attribution"
  LEASE_RUN="$(new_run_id)"
  lane_record "$LEASE_REQUESTED_SLOT" request
  [ ! -e /run/subyard-e2e-lease.json ] && [ ! -L /run/subyard-e2e-lease.json ] ||
    die 'a guest lease marker already exists on the runner'
  # Retain this empty directory after release so another acquisition cannot
  # advertise the used host as a new disposable environment in the same job.
  sudo -n mkdir -m 0700 /run/subyard-e2e-runner/used || die 'this hosted runner already served a lease'
  LEASE_SLOT="$LEASE_REQUESTED_SLOT" LEASE_GENERATION=1 LEASE_EPOCH=1 LEASE_ID="$LEASE_RUN"
  ENVIRONMENT_TYPE=runner
  BASE_FINGERPRINT="$(sha256sum /imagegeneration/imagedata.json | cut -d' ' -f1)"
  GUEST_IDENTITY="$LOCAL_TEMP/lease_id_ed25519"
  ssh-keygen -q -t ed25519 -N '' -C subyard-runner-lease -f "$GUEST_IDENTITY"
  sudo -n install -m 0600 "$GUEST_IDENTITY.pub" /run/subyard-e2e-runner/used/authorized_keys
  sudo -n install -m 0600 /dev/stdin /run/subyard-e2e-runner/used/sshd_config <<'CONFIG'
ListenAddress 127.0.0.1
Port 22222
HostKey /etc/ssh/ssh_host_ed25519_key
AuthorizedKeysFile /run/subyard-e2e-runner/used/authorized_keys
PidFile /run/subyard-e2e-runner/used/sshd.pid
AllowUsers root
PermitRootLogin prohibit-password
AuthenticationMethods publickey
DisableForwarding yes
UsePAM yes
CONFIG
  sudo -n systemd-run --quiet --unit="subyard-runner-lease-$LEASE_RUN" --property=Type=exec \
    /usr/sbin/sshd -D -e -f /run/subyard-e2e-runner/used/sshd_config
  CLIENT_CONFIG="$LOCAL_TEMP/ssh_config"
  BASTION_HOSTNAME=127.0.0.1 BASTION_PORT=22222 BASTION_HOST_KEY_ALIAS=e2e-vm-1
  BASTION_KNOWN_HOSTS="$LOCAL_TEMP/guest_known_hosts"
  printf 'e2e-vm-1 %s\n' "$(normalized_public_key_file /etc/ssh/ssh_host_ed25519_key.pub)" > "$BASTION_KNOWN_HOSTS"
  render_facade_host e2e-vm-1 root "$GUEST_IDENTITY" > "$CLIENT_CONFIG"
  printf '    ConnectionAttempts 5\n    ConnectTimeout 5\n' >> "$CLIENT_CONFIG"
  # Exercise the same real SSH path that the staged fixture and direct P0
  # callers will use before issuing a grant.
  facts="$(E2E_GUEST_TIMEOUT=30 guest 1 sh -ec '. /etc/os-release; printf "image=%s-%s arch=%s" "$ID" "$VERSION_ID" "$(dpkg --print-architecture)"' </dev/null)" ||
    die 'hosted runner SSH identity probe failed'
  [[ "$facts" =~ ^image=ubuntu-24\.04' arch='(amd64|arm64)$ ]] || die 'unsupported hosted runner guest identity'
  jq -cn --arg run "$LEASE_RUN" --arg slot "$LEASE_SLOT" --arg yard "$LEASE_YARD" \
    --arg project "$LEASE_PROJECT" --arg purpose "$LEASE_PURPOSE" \
    '{schema_version:2,$run,$slot,$yard,$project,$purpose}' |
    sudo -n install -m 0444 /dev/stdin /run/subyard-e2e-lease.json
  lane_record "$LEASE_SLOT" grant type=runner vms=1 "base=$BASE_FINGERPRINT" "$facts"
}

start_lease_keeper() { :; } # The allocation belongs to the job.

# Use the client's existing timed release and evidence path.
facade_request() {
  [ -n "$LEASE_ID" ] && [ "$1" = "$(lease_command release)" ] || return 2
  local unit="subyard-runner-lease-$LEASE_RUN.service" state rc=0
  # Stop the scoped listener before revoking its key or releasing the marker.
  state="$(systemctl show "$unit" -p LoadState --value)" || rc=1
  if [ "$rc" = 0 ] && [ "$state" != not-found ]; then
    sudo -n systemctl stop "$unit" || rc=1
  fi
  if [ "$rc" = 0 ]; then
    sudo -n rm -f /run/subyard-e2e-runner/used/authorized_keys /run/subyard-e2e-runner/used/sshd_config /run/subyard-e2e-runner/used/sshd.pid || rc=1
    if [ -e /run/subyard-e2e-lease.json ]; then
      jq -e --arg run "$LEASE_RUN" --arg slot "$LEASE_SLOT" '.run == $run and .slot == $slot' \
        /run/subyard-e2e-lease.json >/dev/null && sudo -n rm /run/subyard-e2e-lease.json || rc=1
    fi
  fi
  [ "$rc" = 0 ] || return "$rc"
  printf '%s\n' '{"status":"ok"}'
}
