#!/usr/bin/env bash
# Run the production controller and client against a deterministic broker facade.
set -euo pipefail
repo="$1" scenario="$2"
shift 2
# shellcheck source=dev/agent-e2e.sh
. "$repo/dev/agent-e2e.sh"
fixture="$(mktemp -d /tmp/subyard-release-lease.XXXXXX)"
trap 'find "$fixture" -depth -delete' EXIT
(
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --slot) allocation_option "$@"; shift 2 ;;
      --lane) shift 2 ;;
      --split-covered|p0-full|p0-split|fixture) shift ;;
      *) exit 99 ;;
    esac
  done
  STATE_ROOT="$fixture/state"
  IDENTITY="$fixture/identity"
  resolve_bastion_route() { :; }
  lease_attribution() { printf 'yard\tProject\n'; }
  facade_request() {
    local frame port='process-acquire-v4'
    if [ -n "${LEASE_FIXTURE_TRACE:-}" ]; then
      printf '%s\n' "${1%% *}" >> "$LEASE_FIXTURE_TRACE"
    fi
    case "$1" in
      status)
        case "$scenario" in released|lease_lost|unavailable|v4-pass|invalid-grant) ;; *) port='unused' ;; esac
        printf '{"schema_version":1,"status":"ok","capabilities":["attribution-v2","environment-acquire-v3","disposable-v1","%s"]}\n' "$port"
        ;;
      acquire-v4\ *)
        printf '{"schema_version":1,"status":"reserved","grant":{"slot_id":"%s","lease_id":"aabb","lease_epoch":1,"capability":"ccdd"}}\n' "$LEASE_REQUESTED_SLOT"
        IFS= read -r frame
        [ "$frame" = keepalive ] || return 21
        case "$scenario" in
          v4-pass|invalid-grant)
            fixture_grant
            while IFS= read -r frame; do [ "$frame" != ack ] || return 0; done
            return 21 ;;
          *) return 23 ;;
        esac
        ;;
      acquire-v3\ *)
        case "$scenario" in
          capacity-then-pass)
            if [ -f "$fixture/refused" ]; then fixture_grant
            else
              touch "$fixture/refused"
              printf '%s\n' '{"schema_version":1,"status":"error","code":"capacity","reason":"memory","message":"fixture refusal"}'
            fi ;;
          memory|disk)
            printf '{"schema_version":1,"status":"error","code":"capacity","reason":"%s","message":"fixture refusal"}\n' "$scenario" ;;
          busy)
            printf '%s\n' '{"schema_version":1,"status":"error","code":"busy","state":"provisioning","reason":"provisioning","message":"requested slot is unavailable"}' ;;
          failed-cancellation)
            printf '%s\n' '{"schema_version":1,"status":"error","code":"unavailable","message":"admission cancellation failed"}' ;;
          *) fixture_grant ;;
        esac
        ;;
      release\ *)
        [[ "$1" == *' aabb 1 ccdd' ]] || return 22
        case "$scenario" in
          lease_lost|unavailable)
            printf '{"schema_version":1,"status":"error","code":"%s"}\n' "$scenario" ;;
          *) printf '%s\n' '{"schema_version":1,"status":"ok","message":"released"}' ;;
        esac
        ;;
      *) return 24 ;;
    esac
  }
  fixture_grant() {
    local data_user="subyard-e2e-slot-$((10#${LEASE_REQUESTED_SLOT#slot-}))"
    [ "$scenario" != invalid-grant ] || data_user='invalid-user'
    jq -nc --arg slot "$LEASE_REQUESTED_SLOT" --arg run "$LEASE_RUN" --arg data_user "$data_user" '
      {schema_version:1,status:"ok",grant:{
        slot_id:$slot,lease_id:"aabb",lease_epoch:1,capability:"ccdd",resource_generation:1,data_user:$data_user,
        context:{schema_version:2,yard:"yard",project:"Project",run:$run,purpose:"agent-e2e"},
        environment:{type:"subyard-pair",vm_count:2,cpu_per_vm:4,
          memory_per_vm:"4GiB",disk_per_vm:"20GiB",lifecycle:"disposable-v1"},
        base_fingerprint:("a" * 64),
        targets:[range(1;3) | {selector:.,name:"e2e-vm-\(.)",address:"10.42.1.1\(.)",
          host_key_type:"ssh-ed25519",host_key_blob:"YWJjZA=="}]}}'
  }
  acquire_run_lease
  for vm in 1 2; do phase_marker guest-cleanup end 0 0 "$vm"; done
  exit "${LEASE_FIXTURE_EXIT:-0}"
)
