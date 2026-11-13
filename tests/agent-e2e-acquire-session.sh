#!/usr/bin/env bash
# JSON fixtures are exported as literal data to the child shell.
# shellcheck disable=SC2089,SC2090
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-acquire-session.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
# Capacity fixtures must fail fast unless they explicitly request a wait.
unset SUBYARD_E2E_CAPACITY_WAIT
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

reservation_json='{"schema_version":1,"status":"reserved","grant":{"slot_id":"slot-001","lease_id":"aabb","lease_epoch":1,"capability":"ccdd"}}'
ready='{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-001","lease_id":"aabb","lease_epoch":1,"capability":"ccdd"}}'
export reservation_json ready

# Exercise the actual parent/guard/session functions, including stdin frames.
for scenario in ready disconnect; do
  (
    LOCAL_TEMP="$TMP/$scenario"
    install -d -m 0700 "$LOCAL_TEMP"
    LEASE_REQUESTED_SLOT='slot-001'
    LEASE_RUN=fixture
    facade_request() {
      local frame
      [ "$3" = stream ] || exit 20
      printf '%s\n' "$reservation_json"
      IFS= read -r frame
      [ "$frame" = keepalive ] || exit 21
      if [ "$scenario" = disconnect ]; then return 23; fi
      # Early credentials must be available in the parent before ready arrives.
      while [ ! -e "$LOCAL_TEMP/parent-has-grant" ]; do sleep 0.1; done
      printf '%s\n' "$ready"
      while IFS= read -r frame; do [ "$frame" != ack ] || return 0; done
      return 22
    }
    # The real session function runs in this shell so its credentials survive.
    if [ "$scenario" = ready ]; then
      (while [ ! -s "$LOCAL_TEMP/acquire-replies" ]; do sleep 0.1; done; touch "$LOCAL_TEMP/parent-has-grant") &
      lease_acquire_stream 'acquire-v4 fixture' || exit 24
      [ "$LEASE_ID:$LEASE_CAPABILITY:$LEASE_EPOCH" = aabb:ccdd:1 ] || exit 25
      [ "$ACQUIRE_RESPONSE" = "$ready" ] || exit 26
      finish_acquire_session || exit 27
    else
      if lease_acquire_stream 'acquire-v4 fixture'; then exit 28; fi
      [ "$LEASE_ID:$LEASE_CAPABILITY" = aabb:ccdd ] || exit 29
    fi
  ) || fail "$scenario parent/session contract"
done

# Exercise acquire_lease from a fresh controller state. Its first public-key
# fingerprint read must follow the existing identity preparation/validation.
# A v4 admission refusal then confirms that the early reservation was aborted.
(
  LOCAL_TEMP="$TMP/capacity"
  STATE_ROOT="$TMP/capacity-client"
  IDENTITY="$TMP/capacity-key"
  install -d -m 0700 "$LOCAL_TEMP"
  LEASE_REQUESTED_SLOT='slot-001'
  LEASE_RUN=abcdef12
  LEASE_PURPOSE=capacity-fixture
  resolve_bastion_route() { :; }
  resolve_workspace_attribution() { printf 'yard\tProject\n'; }
  telemetry_correlation() { :; }
  facade_request() {
    local frame
    if [ "$1" = status ]; then
      [ -s "$IDENTITY" ] && [ -s "$IDENTITY.pub" ] || return 30
      ssh-keygen -lf "$IDENTITY.pub" -E sha256 >/dev/null || return 30
      printf '%s\n' '{"schema_version":1,"status":"ok","capabilities":["attribution-v2","environment-acquire-v3","process-acquire-v4","disposable-v1"]}'
    else
      [[ "$1" == acquire-v4\ * ]] || return 31
      [[ "$1" == *' SHA256:'* ]] || return 31
      printf '%s\n' "$reservation_json"
      IFS= read -r frame
      printf '%s\n' '{"schema_version":1,"status":"error","code":"capacity","reason":"memory","message":"fixture capacity refusal"}'
    fi
  }
  rc=0
  acquire_lease || rc=$?
  [ "$rc" = 4 ] && [ -z "$LEASE_ID" ] && [ -z "$LEASE_ACQUIRE_PID" ] || exit 32
) || fail 'v4 admission refusal retained an aborted lease'

# Run acquire_lease with one final frame and guard exit status. "drained" makes the
# guard exit after the parent saw EOF but before its liveness check, so the final
# frames are drained from disk; "live" holds the guard until the parent waits for it.
late_frame_case() {
  local name="$1" rc=0
  # Distinct names: the dynamically scoped transport must not see acquire locals.
  fixture_final="$2" fixture_guard_rc="$3" fixture_mode="$4"
  LOCAL_TEMP="$TMP/$name"
  STATE_ROOT="$TMP/$name-client"
  IDENTITY="$TMP/$name-key"
  install -d -m 0700 "$LOCAL_TEMP"
  LEASE_REQUESTED_SLOT='slot-001'
  LEASE_RUN=abcdef13
  LEASE_PURPOSE=capacity-fixture
  resolve_bastion_route() { :; }
  resolve_workspace_attribution() { printf 'yard\tProject\n'; }
  telemetry_correlation() { :; }
  write_client_config() { :; }
  start_lease_keeper() { :; }
  facade_request() {
    local frame attempt
    if [ "$1" = status ]; then
      printf '%s\n' '{"schema_version":1,"status":"ok","capabilities":["attribution-v2","environment-acquire-v3","process-acquire-v4","disposable-v1"]}'
      return
    fi
    if [ "$fixture_mode" = drained ]; then
      while [ ! -e "$LOCAL_TEMP/parent-saw-eof" ]; do sleep 0.05; done
    fi
    printf '%s\n' "$reservation_json"
    IFS= read -r frame
    if [ "$fixture_mode" = drained ]; then
      # The final frame has no newline; the stream end still completes it.
      printf '%s' "$fixture_final"
      return "$fixture_guard_rc"
    fi
    printf '%s\n' "$fixture_final"
    for ((attempt=0; attempt<200; attempt++)); do
      [ ! -e "$LOCAL_TEMP/parent-waits" ] || return "$fixture_guard_rc"
      sleep 0.05
    done
    return 99
  }
  if [ "$fixture_mode" = drained ]; then
    # Hold the parent's first liveness check until the guard has exited.
    kill() {
      if [ "$1" = -0 ] && [ -n "${2:-}" ] && [ "$2" = "${LEASE_ACQUIRE_PID:-}" ] &&
        [ ! -e "$LOCAL_TEMP/parent-saw-eof" ]; then
        : > "$LOCAL_TEMP/parent-saw-eof"
        while lease_process_identity "$2" >/dev/null; do sleep 0.05; done
        return 1
      fi
      builtin kill "$@"
    }
  else
    # Release the guard only once the parent waits for the still-live guard.
    wait() {
      if [ -n "${1:-}" ] && [ "$1" = "${LEASE_ACQUIRE_PID:-}" ]; then : > "$LOCAL_TEMP/parent-waits"; fi
      builtin wait "$@"
    }
  fi
  acquire_lease 2> "$LOCAL_TEMP/stderr" || rc=$?
  return "$rc"
}
capacity_disk='{"schema_version":1,"status":"error","code":"capacity","reason":"disk","message":"fixture capacity refusal"}'

# Capacity frames landing before the guard exit must be drained, not declared unknown.
(
  rc=0
  late_frame_case late-capacity "$capacity_disk" 0 drained || rc=$?
  [ "$rc" = 4 ] && [ -e "$LOCAL_TEMP/parent-saw-eof" ] && [ -z "$LEASE_ID" ] && [ -z "$LEASE_ACQUIRE_PID" ] &&
    grep -Fqx 'agent-e2e: retryable capacity refusal for slot-001: resource=disk; retry after resources become available' "$LOCAL_TEMP/stderr" ||
    { cat "$LOCAL_TEMP/stderr" >&2; exit 33; }
) || fail 'capacity frames landing before the guard exit were declared unknown'

# A refusal whose guard failed, reaped while draining or still live, is unknown.
# die exits the case subshell, so its status is the subshell status.
for mode in drained live; do
  rc=0
  (late_frame_case "refusal-guard-$mode" "$capacity_disk" 23 "$mode") || rc=$?
  marker=parent-waits
  [ "$mode" = live ] || marker=parent-saw-eof
  [ "$rc" = 2 ] && [ -e "$TMP/refusal-guard-$mode/$marker" ] &&
    grep -Fqx 'agent-e2e: lease acquire unknown-outcome cause=refusal-guard-rc=23' "$TMP/refusal-guard-$mode/stderr" ||
    { cat "$TMP/refusal-guard-$mode/stderr" >&2; fail "refusal guard failure ($mode) rc=$rc lost its unknown outcome or cause"; }
done

# A grant drained after the guard exited 0 was never acked, so the broker did not hand it off.
# The wrapper EXIT trap must then release it with the granted credentials.
rc=0
full_grant='{"schema_version":1,"status":"ok","grant":{"environment":{"type":"subyard-pair","vm_count":2,"cpu_per_vm":4,"memory_per_vm":"4GiB","disk_per_vm":"20GiB","lifecycle":"disposable-v1"},"base_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","slot_id":"slot-001","resource_generation":1,"lease_id":"aabb","lease_epoch":1,"capability":"ccdd","context":{"schema_version":2,"yard":"yard","project":"Project","run":"abcdef13","purpose":"capacity-fixture"},"data_user":"subyard-e2e-slot-1","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.1.11","host_key_type":"ssh-ed25519","host_key_blob":"YWJjZA=="},{"selector":2,"name":"e2e-vm-2","address":"10.42.1.12","host_key_type":"ssh-ed25519","host_key_blob":"YWJjZA=="}]}}'
(
  release_lease() {
    [ "$LEASE_ID:$LEASE_CAPABILITY:$LEASE_EPOCH" = aabb:ccdd:1 ] || return 1
    : > "$TMP/grant-guard-gone/release-attempted"
  }
  trap cleanup_on_exit EXIT
  late_frame_case grant-guard-gone "$full_grant" 0 drained
) 2> "$TMP/grant-guard-gone.cleanup" || rc=$?
[ "$rc" = 2 ] && [ -e "$TMP/grant-guard-gone/parent-saw-eof" ] && [ -e "$TMP/grant-guard-gone/release-attempted" ] &&
  grep -Fqx 'agent-e2e: lease acquire handoff is unknown; cleanup required' "$TMP/grant-guard-gone/stderr" ||
  { cat "$TMP/grant-guard-gone/stderr" >&2; fail "grant after guard exit rc=$rc was accepted as handed off"; }

# A guard that exits without a terminal frame stays unknown and names its cause.
(
  LOCAL_TEMP="$TMP/no-frame"
  install -d -m 0700 "$LOCAL_TEMP"
  LEASE_REQUESTED_SLOT='slot-001'
  LEASE_RUN=fixture
  # shellcheck disable=SC2317 # Invoked by the guard.
  facade_request() { printf '%s\n' "$reservation_json"; return 23; }
  if lease_acquire_stream 'acquire-v4 fixture'; then exit 34; fi
  [ "$LEASE_ACQUIRE_FAILURE" = 'stream-ended-no-frame guard-rc=23' ] && [ -z "$LEASE_ACQUIRE_PID" ] || exit 35
) || fail 'stream without a terminal frame lost its unknown outcome or cause'

# The physical fixture must retain the actual child exit and bounded error
# classes before teardown, while dropping private paths and arbitrary payloads.
(
  eval "$(sed -n '/^pregrant_child_failure() {/,/^}/p; /^holder_failure() {/,/^}/p' "$ROOT/config/profiles/test-vms/tests/e2e/broker-memory-boundary.sh")"
  STATE_PARENT="$TMP/client-diagnostics"
  install -d -m 0700 "$STATE_PARENT"
  printf '%s\n' 'ssh-keygen: /fixture/private/controller.pub: No such file or directory' \
    'agent-e2e: lease acquire unknown-outcome cause=stream-ended-no-frame guard-rc=23 owner-or-requester-lost' \
    'agent-e2e: lease acquire unknown-outcome cause=/fixture/private' \
    'agent-e2e: lease acquire unknown-outcome cause=fixture-secret capability=ccdd' \
    'agent-e2e: lease acquire unknown-outcome cause=transport-rc=1 fixture-secret' \
    'grant capability=fixture-secret' > "$STATE_PARENT/pregrant-TERM.log"
  (exit 23) &
  # shellcheck disable=SC2034 # Consumed by the extracted child failure handler.
  HOLDER_PID=$!
  pregrant_child_failure pregrant-TERM 'pregrant client exited before cancellation'
) > "$TMP/client-diagnostics.log" 2>&1 && fail 'child failure lost fixture status' || diagnostics_rc=$?
[ "$diagnostics_rc" = 2 ] &&
  grep -Fq 'pregrant child outcome: client=pregrant-TERM exit_code=23' "$TMP/client-diagnostics.log" &&
  grep -Fq 'controller public identity unavailable before acquire' "$TMP/client-diagnostics.log" &&
  grep -Fqx 'agent-e2e: lease acquire unknown-outcome cause=stream-ended-no-frame guard-rc=23 owner-or-requester-lost' "$TMP/client-diagnostics.log" &&
  ! grep -Eq '/fixture/private|fixture-secret|ccdd' "$TMP/client-diagnostics.log" \
  || fail 'child diagnostics lost native exit or disclosed untrusted detail'

# SIGKILL cannot run the wrapper EXIT trap. Its existing process watcher must
# end the owned SSH group and attempt cleanup with the early capability.
cat > "$TMP/owner.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
. "$ROOT/dev/agent-e2e.sh"
LOCAL_TEMP="$FIXTURE"
LEASE_REQUESTED_SLOT='slot-001'
LEASE_RUN=fixture
facade_request() {
  local frame
  printf '%s\n' "$reservation_json"
  printf '%s\n' "$BASHPID" > "$FIXTURE/transport-pid"
  while IFS= read -r frame; do :; done
}
release_lease() {
  [ "$LEASE_ID:$LEASE_CAPABILITY:$LEASE_EPOCH" = aabb:ccdd:1 ] || return 1
  touch "$FIXTURE/release-confirmed"
}
lease_acquire_stream 'acquire-v4 fixture'
EOF
install -d -m 0700 "$TMP/death"
ROOT="$ROOT" FIXTURE="$TMP/death" bash "$TMP/owner.sh" > "$TMP/death.log" 2>&1 &
owner=$!
for ((attempt=0; attempt<100; attempt++)); do
  [ ! -s "$TMP/death/transport-pid" ] || break
  sleep 0.05
done
[ -s "$TMP/death/transport-pid" ] || fail 'owned transport did not start'
kill -KILL "$owner"
wait "$owner" 2>/dev/null || true
for ((attempt=0; attempt<100; attempt++)); do
  [ ! -e "$TMP/death/release-confirmed" ] || break
  sleep 0.05
done
[ -e "$TMP/death/release-confirmed" ] || { cat "$TMP/death.log"; fail 'wrapper death retained pregrant ownership'; }
transport="$(cat "$TMP/death/transport-pid")"
! lease_process_identity "$transport" >/dev/null || fail 'wrapper death retained owned SSH transport'
(
  LEASE_ACQUIRE_PID=999999
  LEASE_ID=''
  LOCAL_TEMP=''
  LEASE_KEEPER_PID=''
  LEASE_REQUESTED_SLOT='slot-001'
  LEASE_RUN=unknown-fixture
  GUEST_DIRS=()
  cleanup_on_exit interrupted 143
) > "$TMP/unknown.log" 2>&1 && fail 'interrupted acquire lost its native status' || rc=$?
[ "$rc" = 143 ] && grep -Fq 'acquire cleanup remains unconfirmed' "$TMP/unknown.log" &&
  grep -Fq 'phase=cleanup/release state=end duration_seconds=0 exit_code=3' "$TMP/unknown.log" \
  || fail 'unknown server cleanup was reported as successful'
printf 'ok: streaming acquire ownership, handoff, disconnect and wrapper death\n'
