#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
RUNNER="$ROOT/dev/agent-e2e.sh"
YARD="${SUBYARD_E2E_YARD:-test-yard}"
STATE_PARENT=''
HOLDER_A=''
HOLDER_B=''

die() { printf 'p1-lease-acceptance: %s\n' "$*" >&2; exit 2; }

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  [ -z "$HOLDER_A" ] || kill "$HOLDER_A" >/dev/null 2>&1
  [ -z "$HOLDER_B" ] || kill "$HOLDER_B" >/dev/null 2>&1
  [ -z "$HOLDER_A" ] || wait "$HOLDER_A" >/dev/null 2>&1
  [ -z "$HOLDER_B" ] || wait "$HOLDER_B" >/dev/null 2>&1
  if [ -n "$STATE_PARENT" ]; then
    case "$STATE_PARENT" in /tmp/subyard-p1-lease.*|"${TMPDIR:-/tmp}"/subyard-p1-lease.*)
      find "$STATE_PARENT" -depth -delete >/dev/null 2>&1
      ;;
    esac
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

command -v jq >/dev/null 2>&1 || die 'jq is required'
command -v ssh >/dev/null 2>&1 || die 'ssh is required'
STATE_PARENT="$(mktemp -d "${TMPDIR:-/tmp}/subyard-p1-lease.XXXXXX")"
for client in a b c; do
  SUBYARD_E2E_STATE_DIR="$STATE_PARENT/$client" "$RUNNER" --yard "$YARD" --prepare >/dev/null
done

status() {
  SUBYARD_E2E_STATE_DIR="$STATE_PARENT/c" "$RUNNER" --yard "$YARD" --status --json
}

dump_broker_diagnostics() {
  local incident payload
  sudo -n systemctl start subyard-test-vms-host-sink.service >/dev/null 2>&1 || true
  payload="$(status 2>/dev/null)" || payload=''
  [ -z "$payload" ] || printf '%s\n' "$payload" >&2
  "$ROOT/bin/yard" test-vms logs -n 200 >&2 || true
  incident="$(jq -r '
    [.pool.slots[] | select(.incident_id != null) | .incident_id] | last // empty
  ' <<<"$payload" 2>/dev/null)"
  [ -z "$incident" ] || {
    printf 'p1-lease-acceptance: incident %s\n' "$incident" >&2
    jq . "${SUBYARD_HOME:-$HOME/.subyard}/logs/test-vms-broker-incidents/$incident.json" \
      >&2 2>/dev/null || true
  }
}

wait_for_state_count() {
  local state="$1" expected="$2" attempts="${3:-90}" payload count
  for _ in $(seq 1 "$attempts"); do
    payload="$(status)"
    count="$(jq --arg state "$state" '[.pool.slots[] | select(.state == $state)] | length' \
      <<<"$payload")"
    if [ "$count" = "$expected" ]; then
      printf '%s\n' "$payload"
      return 0
    fi
    if jq -e '.pool.slots[] | select(.state == "quarantined")' <<<"$payload" >/dev/null; then
      printf '%s\n' "$payload" >&2
      return 1
    fi
    sleep 2
  done
  printf '%s\n' "$payload" >&2
  return 1
}

wait_for_ready() {
  local client="$1" pid="$2" attempts="${P1_LEASE_READY_ATTEMPTS:-900}"
  for _ in $(seq 1 "$attempts"); do
    [ ! -s "$STATE_PARENT/$client.ready" ] || return 0
    [ ! -s "$STATE_PARENT/$client.failed" ] \
      || { sed -n '1,240p' "$STATE_PARENT/$client.log" >&2;
           dump_broker_diagnostics; return 1; }
    kill -0 "$pid" >/dev/null 2>&1 \
      || { sed -n '1,240p' "$STATE_PARENT/$client.log" >&2;
           dump_broker_diagnostics; return 1; }
    sleep 1
  done
  sed -n '1,240p' "$STATE_PARENT/$client.log" >&2
  dump_broker_diagnostics
  return 1
}

wait_for_slot_available() {
  local slot="$1" attempts="${2:-180}" payload state
  for _ in $(seq 1 "$attempts"); do
    payload="$(status)"
    state="$(jq -r --arg slot "$slot" '
      .pool.slots[] | select(.slot_id == $slot) | .state
    ' <<<"$payload")"
    [ "$state" != available ] || return 0
    case "$state" in quarantined | recovering | provisioning) ;;
      *) printf '%s\n' "$payload" >&2; return 1 ;;
    esac
    sleep 2
  done
  printf '%s\n' "$payload" >&2
  return 1
}

wait_for_guest_access() {
  local client="$1" config="$2" attempts="${P1_LEASE_SSH_ATTEMPTS:-12}"
  local log="$STATE_PARENT/$client-ssh.log"
  for _ in $(seq 1 "$attempts"); do
    if ssh -F "$config" -T -o ConnectTimeout=5 e2e-vm-1 -- true \
      </dev/null >"$log" 2>&1; then
      rm -f "$log"
      return 0
    fi
    sleep 2
  done
  sed -n '1,120p' "$log" >&2
  return 1
}

hold_lease() (
  local client="$1" purpose="$2" requested_slot="$3" ready_temp
  local before_boot after_boot='' down=0 up=0
  export SUBYARD_E2E_STATE_DIR="$STATE_PARENT/$client"
  export SUBYARD_E2E_YARD="$YARD"
  # shellcheck source=dev/agent-e2e.sh
  . "$RUNNER"

  LOCAL_TEMP="$(mktemp -d "$STATE_PARENT/$client-runtime.XXXXXX")"
  LEASE_PURPOSE="$purpose"
  LEASE_REQUESTED_SLOT="$requested_slot"
  holder_cleanup() {
    local rc=$? release_failed=0
    trap - EXIT INT TERM
    set +e
    if [ -n "$LEASE_KEEPER_PID" ]; then
      kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      LEASE_KEEPER_PID=''
    fi
    release_lease || release_failed=1
    [ "$release_failed" = 0 ] || rc=1
    exit "$rc"
  }
  trap holder_cleanup EXIT
  trap 'exit 143' INT TERM

  acquire_lease || { : > "$STATE_PARENT/$client.failed"; exit 1; }
  start_lease_keeper
  printf '%s\n' \
    'set -eu' \
    'jq -e --arg yard "$1" --arg project "$2" --arg run "$3" --arg purpose "$4" '\''.schema_version == 2 and .yard == $yard and .project == $project and (has("checkout") | not) and .run == $run and .purpose == $purpose'\'' /run/subyard-e2e-lease.json >/dev/null' \
    | guest 1 sh -s -- "$LEASE_YARD" "$LEASE_PROJECT" "$LEASE_RUN" "$LEASE_PURPOSE"
  guest 2 jq -e \
    --arg yard "$LEASE_YARD" \
    --arg project "$LEASE_PROJECT" \
    --arg run "$LEASE_RUN" \
    --arg purpose "$LEASE_PURPOSE" \
    '.schema_version == 2 and .yard == $yard and .project == $project and
      (has("checkout") | not) and .run == $run and .purpose == $purpose' \
    /run/subyard-e2e-lease.json >/dev/null

  if [ "$client" = b ] && [ "${P1_LEASE_REBOOT_VERIFY:-1}" = 1 ]; then
    before_boot="$(guest 1 cat /proc/sys/kernel/random/boot_id)"
    guest 1 systemctl reboot >/dev/null 2>&1 || true
    for _ in $(seq 1 60); do
      if ! ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=2 e2e-vm-1 -- true \
        </dev/null >/dev/null 2>&1; then
        down=1
        break
      fi
      sleep 1
    done
    [ "$down" = 1 ] || die 'leased guest did not go down for reboot'
    for _ in $(seq 1 180); do
      after_boot="$(ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- \
        cat /proc/sys/kernel/random/boot_id 2>/dev/null)" || after_boot=''
      if [ -n "$after_boot" ] && [ "$after_boot" != "$before_boot" ]; then
        up=1
        break
      fi
      sleep 1
    done
    [ "$up" = 1 ] || die 'leased guest did not return with a new boot ID'
    guest 1 jq -e \
      --arg yard "$LEASE_YARD" \
      --arg project "$LEASE_PROJECT" \
      --arg run "$LEASE_RUN" \
      --arg purpose "$LEASE_PURPOSE" \
      '.schema_version == 2 and .yard == $yard and .project == $project and
        .run == $run and .purpose == $purpose' \
      /run/subyard-e2e-lease.json >/dev/null
    guest 1 systemctl is-enabled --quiet subyard-e2e-lease-context.service
    guest 1 ip route show default | grep -q .
  fi

  umask 077
  printf '%s\n' "$(lease_command renew)" > "$STATE_PARENT/$client.stale-renew"
  ready_temp="$(mktemp "$STATE_PARENT/.$client.ready.XXXXXX")"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$LEASE_SLOT" "$LEASE_YARD" "$LEASE_PROJECT" "$LEASE_RUN" "$LEASE_PURPOSE" \
    "$CLIENT_CONFIG" "$GUEST_IDENTITY" "$GUEST_KNOWN_HOSTS" "${VM_IP[1]}" "${VM_IP[2]}" \
    > "$ready_temp"
  mv -f "$ready_temp" "$STATE_PARENT/$client.ready"
  while [ ! -e "$STATE_PARENT/$client.release" ]; do sleep 1; done
)

start_holder_with_recovery() {
  local client="$1" purpose="$2" requested_slot="$3"
  local attempt=1 pid payload state marker
  while true; do
    hold_lease "$client" "$purpose" "$requested_slot" \
      >"$STATE_PARENT/$client.log" 2>&1 &
    pid=$!
    case "$client" in
      a) HOLDER_A="$pid" ;;
      b) HOLDER_B="$pid" ;;
      *) die "unknown holder client $client" ;;
    esac
    if wait_for_ready "$client" "$pid"; then
      return 0
    fi
    wait "$pid" >/dev/null 2>&1 || true
    case "$client" in
      a) HOLDER_A='' ;;
      b) HOLDER_B='' ;;
    esac
    grep -Fq 'lease acquire failed (quarantined: slot provisioning failed)' \
      "$STATE_PARENT/$client.log" || return 1
    payload="$(status)"
    state="$(jq -r --arg slot "$requested_slot" '
      .pool.slots[] | select(.slot_id == $slot) | .state
    ' <<<"$payload")"
    case "$state" in available | quarantined | recovering | provisioning) ;;
      *) printf '%s\n' "$payload" >&2; return 1 ;;
    esac
    printf 'p1-lease-acceptance: %s provisioning attempt %s entered %s; waiting for automatic rebuild\n' \
      "$requested_slot" "$attempt" "$state" >&2
    [ "$attempt" -lt 3 ] || { printf '%s\n' "$payload" >&2; return 1; }
    wait_for_slot_available "$requested_slot" \
      || return 1
    for marker in \
      "$STATE_PARENT/$client.ready" \
      "$STATE_PARENT/$client.failed" \
      "$STATE_PARENT/$client.release"; do
      [ ! -e "$marker" ] || find "$marker" -delete
    done
    attempt=$((attempt + 1))
  done
}

stale_renew() (
  local client="$1"
  export SUBYARD_E2E_STATE_DIR="$STATE_PARENT/$client"
  export SUBYARD_E2E_YARD="$YARD"
  # shellcheck source=dev/agent-e2e.sh
  . "$RUNNER"
  facade_request "$(cat "$STATE_PARENT/$client.stale-renew")"
)

prepare_slot_lease() (
  local slot="$1" vm release_failed=0
  export SUBYARD_E2E_STATE_DIR="$STATE_PARENT/c"
  export SUBYARD_E2E_YARD="$YARD"
  # shellcheck source=dev/agent-e2e.sh
  . "$RUNNER"

  LOCAL_TEMP="$(mktemp -d "$STATE_PARENT/prepare-runtime.XXXXXX")"
  LEASE_PURPOSE=acceptance-prepare
  printf -v LEASE_REQUESTED_SLOT 'slot-%03d' "$slot"
  prepare_cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    set +e
    if [ -n "$LEASE_KEEPER_PID" ]; then
      kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      LEASE_KEEPER_PID=''
    fi
    release_lease || release_failed=1
    [ "$release_failed" = 0 ] || rc=1
    exit "$rc"
  }
  trap prepare_cleanup EXIT INT TERM

  acquire_lease
  start_lease_keeper
  for vm in 1 2; do
    guest "$vm" sh -eu -c \
      'sync
       fstrim -av >/dev/null 2>&1 || true'
  done
)

prepare_slot() {
  local slot="$1" rc=0
  prepare_slot_lease "$slot" || rc=$?
  if [ "$rc" -ne 0 ]; then
    dump_broker_diagnostics
    return "$rc"
  fi
}

initial="$(status)"
jq -e '
  (.pool.slots | length) == 2 and
  all(.pool.slots[]; .state == "available")
' <<<"$initial" >/dev/null \
  || die 'run only against an empty two-slot candidate broker'

# First boot performs package installation in both VMs. Prepare each slot
# sequentially through an ordinary lease so a constrained diagnostic host does
# not run four first-boot package managers at once. The isolation contract
# below still holds both stopped-and-reused pairs concurrently.
for slot in 1 2; do
  prepare_slot "$slot"
done
prepared="$(status)"
jq -e 'all(.pool.slots[]; .state == "available")' <<<"$prepared" >/dev/null \
  || die 'sequential slot preparation did not release the candidate pool'

start_holder_with_recovery b holder-b slot-002 \
  || die 'second holder did not become ready'
wait_for_state_count held 1 >/dev/null \
  || { sed -n '1,240p' "$STATE_PARENT/b.log" >&2; die 'second holder was not published'; }
IFS=$'\t' read -r -a B_READY < "$STATE_PARENT/b.ready"
B_SLOT="${B_READY[0]}"
B_CONFIG="${B_READY[5]}"
B_KNOWN_HOSTS="${B_READY[7]}"
B_IP1="${B_READY[8]}"
[ "$B_SLOT" = slot-002 ] || die "exact holder B received $B_SLOT"

set +e
SUBYARD_E2E_STATE_DIR="$STATE_PARENT/c" \
  "$RUNNER" --yard "$YARD" --slot 2 --purpose exact-slot-busy --ssh 1 -- true \
  >"$STATE_PARENT/exact-busy.log" 2>&1
exact_busy_rc=$?
set -e
[ "$exact_busy_rc" = 2 ] \
  || { sed -n '1,120p' "$STATE_PARENT/exact-busy.log" >&2;
       die "occupied exact-slot acquire returned $exact_busy_rc, want 2"; }
grep -Fq 'requested E2E slot slot-002 is not available' "$STATE_PARENT/exact-busy.log" \
  || die 'occupied exact-slot acquire did not fail explicitly'
exact_busy_state="$(status)"
jq -e '
  (.pool.slots[] | select(.slot_id == "slot-001") | .state) == "available" and
  (.pool.slots[] | select(.slot_id == "slot-002") | .state) == "held"
' <<<"$exact_busy_state" >/dev/null \
  || die 'occupied exact-slot acquire fell back or changed its neighbor'

start_holder_with_recovery a holder-a slot-001 \
  || die 'first holder did not become ready'
held="$(wait_for_state_count held 2)" \
  || { sed -n '1,240p' "$STATE_PARENT/a.log" >&2; die 'first holder was not published'; }
IFS=$'\t' read -r -a A_READY < "$STATE_PARENT/a.ready"
A_SLOT="${A_READY[0]}"
A_YARD="${A_READY[1]}"
A_PROJECT="${A_READY[2]}"
A_CONFIG="${A_READY[5]}"
A_KEY="${A_READY[6]}"
[ "$A_SLOT" = slot-001 ] || die "exact holder A received $A_SLOT"

jq -e '
  ([.pool.slots[] | select(.state == "held") | .slot_id] | sort) ==
    ["slot-001", "slot-002"] and
  ([.pool.slots[] | select(.state == "held") | .project] | unique | length) == 1 and
  ([.pool.slots[] | select(.state == "held") | .purpose] | sort) ==
    ["holder-a", "holder-b"] and
  all(.pool.slots[] | select(.state == "held");
    (.yard | length > 0) and (has("checkout") | not) and
    (.run | test("^[0-9a-f]{8}$"))) and
  all(.pool.slots[];
    (has("client_id") or has("controller_fingerprint") or has("lease_id") or
     has("capability_hash") or has("targets") or has("address") or
     has("host_key_blob")) | not)
' <<<"$held" >/dev/null || die 'held pool is not distinct and redacted'
grep -Eq "^E2E lease: yard=$A_YARD project=$A_PROJECT .* purpose=holder-a slot=$A_SLOT$" \
  "$STATE_PARENT/a.log" \
  || die 'first holder did not print canonical project attribution'
grep -Eq "^E2E lease: yard=$A_YARD project=$A_PROJECT .* purpose=holder-b slot=slot-[0-9]{3}$" \
  "$STATE_PARENT/b.log" || die 'second holder did not print its attributed assignment'
for log in "$STATE_PARENT/a.log" "$STATE_PARENT/b.log"; do
  ! grep '^E2E lease:' "$log" | grep -Eq \
    '([0-9]{1,3}\.){3}[0-9]{1,3}|lease_id|capability|/tmp/|/home/' \
    || die 'assignment banner disclosed an endpoint, credential or private path'
done

wait_for_guest_access a "$A_CONFIG" \
  || die 'holder A could not reach its own guest'
wait_for_guest_access b "$B_CONFIG" \
  || die 'holder B could not reach its own guest'
if ssh -F "$A_CONFIG" -T -o ConnectTimeout=5 -W "$B_IP1:22" subyard-e2e-data \
  </dev/null >/dev/null 2>&1; then
  die 'holder A data account forwarded to holder B'
fi
if ssh -F /dev/null -T -o ConnectTimeout=5 -o HostName="$B_IP1" -o User=root \
  -o IdentityFile="$A_KEY" -o IdentitiesOnly=yes -o BatchMode=yes \
  -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$B_KNOWN_HOSTS" \
  -o HostKeyAlias=e2e-vm-1 -o ProxyJump=none \
  -o "ProxyCommand=ssh -F $B_CONFIG -T -W %h:%p subyard-e2e-data" \
  holder-b-with-a-key -- true </dev/null >/dev/null 2>&1; then
  die 'holder A ephemeral key authenticated to holder B guest'
fi
if printf '%s\n' \
  'set -eu' \
  'if timeout 3 bash -c "</dev/tcp/$1/22" 2>/dev/null; then exit 42; fi' \
  | ssh -F "$A_CONFIG" -T e2e-vm-1 -- bash -s -- "$B_IP1"; then
  :
else
  case "$?" in
    42) die 'holder A guest root reached holder B slot network' ;;
    *) die 'holder A guest-network isolation probe failed unexpectedly' ;;
  esac
fi

set +e
SUBYARD_E2E_STATE_DIR="$STATE_PARENT/c" \
  "$RUNNER" --yard "$YARD" --purpose holder-c --ssh 1 -- true \
  >"$STATE_PARENT/c.log" 2>&1
busy_rc=$?
set -e
[ "$busy_rc" = 4 ] \
  || { sed -n '1,120p' "$STATE_PARENT/c.log" >&2; die "third acquire returned $busy_rc, want 4"; }
grep -Fq 'agent-e2e: pool busy' "$STATE_PARENT/c.log" \
  && grep -Fq "$A_PROJECT" "$STATE_PARENT/c.log" \
  && grep -Fq 'holder-a' "$STATE_PARENT/c.log" \
  && grep -Fq 'holder-b' "$STATE_PARENT/c.log" \
  || { sed -n '1,120p' "$STATE_PARENT/c.log" >&2; die 'busy output omitted holders'; }

: > "$STATE_PARENT/b.release"
if ! wait "$HOLDER_B"; then
  sed -n '1,240p' "$STATE_PARENT/b.log" >&2
  die 'holder B release command failed'
fi
HOLDER_B=''
wait_for_state_count held 1 60 >/dev/null \
  || die 'holder B did not release its slot'
if ssh -F "$B_CONFIG" -T -o ConnectTimeout=5 e2e-vm-1 -- true \
  </dev/null >/dev/null 2>&1; then
  die 'holder B stale SSH configuration survived release'
fi
stale_response="$(stale_renew b)"
[ "$(jq -r '.code // empty' <<<"$stale_response")" = lease_lost ] \
  || die 'holder B stale capability survived release'

sleep 1
: > "$STATE_PARENT/a.release"
if ! wait "$HOLDER_A"; then
  sed -n '1,240p' "$STATE_PARENT/a.log" >&2
  die 'holder A release command failed'
fi
HOLDER_A=''
released="$(wait_for_state_count available 2 60)" \
  || die 'both slots did not return to available'
jq -e 'all(.pool.slots[]; .state == "available")' <<<"$released" >/dev/null \
  || die 'release left a non-available slot'
printf 'ok: attributed holders were isolated, fenced and released\n'
