#!/usr/bin/env bash
# Session lease handles: acquire once, exec in separate processes, release, keeper fencing.
set -euo pipefail
PROFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLIENT="$PROFILE/skills/subyard-test-vms/client.sh"
TMP="$(mktemp -d /tmp/subyard-test-vms-client.XXXXXX)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
stop_keepers() {
  local ready
  for ready in "$TMP"/client/handles/*/keeper.ready; do
    [ -f "$ready" ] && kill -KILL "$(cut -d' ' -f1 "$ready")" >/dev/null 2>&1 || true
  done
}
trap 'stop_keepers; rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin" "$TMP/routes/test-yard/current" "$TMP/vm"
ssh-keygen -q -t ed25519 -N '' -f "$TMP/bastion-key"
read -r key_type key_blob _ < "$TMP/bastion-key.pub"
printf 'subyard-e2e-route-v1\nhostname\t127.0.0.1\nport\t22\nhost_key_alias\tsubyard-e2e-bastion\n' \
  > "$TMP/routes/test-yard/current/route.tsv"
printf 'subyard-e2e-bastion %s %s\n' "$key_type" "$key_blob" > "$TMP/routes/test-yard/current/known_hosts"
printf '%s\n' '{"target":{"yard":{"name":"hermes","project":"subyard-hermes","instance":"yard"}}}' > "$TMP/facts.json"

# The fake transport answers the forced facade and runs guest commands in one
# persistent directory, standing in for the leased VM's root filesystem.
cat > "$TMP/bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
command="${!#}"
host='' config='' previous=''
for argument in "$@"; do
  case "$argument" in subyard-e2e-bastion|e2e-vm-1|e2e-vm-2) host="$argument" ;; esac
  [ "$previous" != -F ] || config="$argument"
  previous="$argument"
done
log() { printf '%s\n' "$*" >> "$FAKE/facade.log"; }
grant() {
  jq -cn --arg yard "$1" --arg project "$2" --arg run "$3" --arg purpose "$4" \
    '{schema_version:1,status:"ok",grant:{environment:{type:"subyard-pair",vm_count:1,cpu_per_vm:2,
      memory_per_vm:"4GiB",disk_per_vm:"20GiB",lifecycle:"disposable-v1"},
      base_fingerprint:("a" * 64),slot_id:"slot-002",resource_generation:1,lease_id:"aabbccdd",
      lease_epoch:2,capability:"eeff0011",data_user:"subyard-e2e-slot-2",
      context:{schema_version:2,yard:$yard,project:$project,run:$run,purpose:$purpose},
      targets:[{selector:1,name:"e2e-vm-1",address:"10.42.2.11",host_key_type:"ssh-ed25519",host_key_blob:"YWJjZA=="}]}}'
}
if [ "$host" != subyard-e2e-bastion ]; then
  [ "$host" = e2e-vm-1 ] || exit 255
  grep -Fq 'User root' "$config" || exit 254
  log "guest-start $command"
  rc=0
  (cd "$FAKE/vm" && bash -c "$command") || rc=$?
  log "guest-end $rc"
  exit "$rc"
fi
case "$command" in
  status)
    log status
    capabilities='["attribution-v2","environment-acquire-v3","disposable-v1","vm-count-v1","disk-size-v1"]'
    [ ! -e "$FAKE/v4" ] || capabilities='["attribution-v2","environment-acquire-v3","disposable-v1","vm-count-v1","disk-size-v1","process-acquire-v4"]'
    jq -cn --argjson capabilities "$capabilities" '{schema_version:1,status:"ok",capabilities:$capabilities,
      pool:{schema_version:2,slots:[{slot_id:"slot-002",state:"available",resource_generation:1,lease_epoch:1}]}}'
    ;;
  acquire-v3\ *|acquire-v4\ *)
    read -r protocol _ _ _ yard project run purpose _ _ slot count _ <<<"$command"
    log "$protocol yard=$yard project=$project purpose=$purpose slot=$slot count=${count:-}"
    case "$(cat "$FAKE/scenario" 2>/dev/null || true)" in
      busy)
        printf '%s\n' '{"schema_version":1,"status":"error","code":"busy","state":"held","reason":"busy","owner":{"display_label":"Other#run-a","yard":"other","project":"Other","run":"run-a","purpose":"tests","acquired_at":"2026-08-24T12:00:00Z","expires_at":"2026-08-24T12:20:00Z"},"message":"requested slot is unavailable"}'
        exit 0 ;;
      capacity)
        printf '%s\n' '{"schema_version":1,"status":"error","code":"capacity","reason":"memory","message":"insufficient memory"}'
        exit 0 ;;
    esac
    if [ "$protocol" = acquire-v4 ]; then
      printf '%s\n' '{"schema_version":1,"status":"reserved","grant":{"slot_id":"slot-002","lease_id":"aabbccdd","lease_epoch":2,"capability":"eeff0011"}}'
    fi
    grant "$yard" "$project" "$run" "$purpose"
    if [ "$protocol" = acquire-v4 ]; then
      while IFS= read -r line; do
        if [ "$line" = ack ]; then
          # The detached keeper must already own the lease when the grant is acknowledged.
          for ready in "$FAKE"/client/handles/*/keeper.ready; do [ -s "$ready" ] && log ack-after-keeper; done
          log ack
          break
        fi
      done
    fi
    ;;
  renew\ *)
    if [ -e "$FAKE/renew-transport-fails" ]; then log renew-transport-failure; exit 255; fi
    if [ -e "$FAKE/renew-delay" ]; then
      # The request is accepted only after transport latency; a killed transport never arrives.
      log connect-renew
      sleep 3
    fi
    log renew
    if [ -e "$FAKE/renew-fails" ]; then
      printf '%s\n' '{"schema_version":1,"status":"error","code":"lease_lost","message":"lease is no longer current"}'
    else
      printf '%s\n' '{"schema_version":1,"status":"ok","expires_at":"2026-08-24T12:20:00Z"}'
    fi
    ;;
  release\ *)
    log release
    if [ -e "$FAKE/release-rejected" ]; then
      printf '%s\n' '{"schema_version":1,"status":"error","code":"quarantined","message":"slot fencing or stop failed"}'
    elif [ -e "$FAKE/renew-fails" ]; then
      printf '%s\n' '{"schema_version":1,"status":"error","code":"lease_lost","message":"lease is no longer current"}'
    else
      printf '%s\n' '{"schema_version":1,"status":"ok","message":"released"}'
    fi
    ;;
  *) log "rejected $command"; printf '%s\n' '{"schema_version":1,"status":"error","code":"invalid_request"}' ;;
esac
EOF
chmod 0755 "$TMP/bin/ssh"

export PATH="$TMP/bin:$PATH" FAKE="$TMP"
export SUBYARD_E2E_TEST_MODE=1 SUBYARD_E2E_TEST_RENEW_SECONDS=1
export SUBYARD_E2E_TEST_RENEW_RETRY_SECONDS=1 SUBYARD_E2E_TEST_RENEW_STALE_SECONDS=5
export SUBYARD_E2E_STATE_DIR="$TMP/client" SUBYARD_E2E_ROUTE_REGISTRY="$TMP/routes"
export SUBYARD_E2E_GUEST_FACTS="$TMP/facts.json"
unset SUBYARD_E2E_ROLLOUT_YARD SUBYARD_E2E_CAPACITY_WAIT
client() { (cd "$TMP/vm" && bash "$CLIENT" "$@"); }
count() { grep -c -- "^$1" "$TMP/facade.log" || true; }
keeper_pid() { cut -d' ' -f1 "$TMP/client/handles/$1/keeper.ready"; }
wait_for() {
  local _
  for _ in $(seq 1 100); do
    if eval "$1"; then return 0; fi
    sleep 0.1
  done
  return 1
}
: > "$TMP/facade.log"

# The idle limit defaults to 20 minutes and is bounded by 2 hours outside test mode.
if SUBYARD_E2E_TEST_MODE=0 client acquire --yard test-yard --slot 2 --idle 3h >/dev/null 2>"$TMP/err"; then
  fail 'acquire accepted an idle limit above 2h'
fi
grep -Fq -- '--idle must be between 1m and 2h' "$TMP/err" || fail 'missing idle bound diagnostic'

# Acquire is explicit about its slot and allocates nothing without it.
if client acquire >/dev/null 2>"$TMP/err"; then fail 'acquire without --slot succeeded'; fi
grep -Fq 'acquire requires --slot N' "$TMP/err" || fail 'missing --slot diagnostic'
[ "$(count acquire)" = 0 ] || fail 'acquire without --slot reached the broker'

# A refused attribution label ends the command before the broker sees a request.
if client acquire --yard test-yard --slot 2 --project 'bad label' >/dev/null 2>"$TMP/err"; then
  fail 'acquire accepted an unsafe project label'
fi
grep -Fq -- '--project must be a safe project label' "$TMP/err" || fail 'missing project label diagnostic'
[ "$(count acquire)" = 0 ] || fail "acquire with an unsafe project label reached the broker: $(cat "$TMP/facade.log")"
[ -z "$(ls -A "$TMP/client/handles")" ] || fail 'a refused project label left a handle'

# One lease spans separate processes: state written by one exec is read by the next.
for protocol in v3 v4; do
  rm -f "$TMP/v4" "$TMP/vm/state"
  [ "$protocol" = v3 ] || touch "$TMP/v4"
  : > "$TMP/facade.log"
  handle="$(client acquire --yard test-yard --slot 2 --purpose skill-check 2>"$TMP/acquire.err")" \
    || { cat "$TMP/acquire.err" >&2; fail "$protocol acquire failed"; }
  [[ "$handle" =~ ^[0-9a-f]{8}$ ]] || fail "$protocol acquire printed no handle: $handle"
  grep -Fq "acquire-$( [ "$protocol" = v4 ] && echo v4 || echo v3) yard=hermes project=hermes purpose=skill-check slot=slot-002 count=1" "$TMP/facade.log" \
    || fail "$protocol acquire lost attribution or the singleton request: $(cat "$TMP/facade.log")"
  if [ "$protocol" = v4 ]; then
    grep -Fxq ack-after-keeper "$TMP/facade.log" || fail 'grant was acknowledged before the detached keeper owned it'
  fi
  dir="$TMP/client/handles/$handle"
  [ "$(jq -r .idle_seconds "$dir/lease.json")" = 1200 ] || fail "$protocol default idle limit is not 20 minutes"
  [ "$(stat -c %a "$dir")" = 700 ] && [ "$(stat -c %a "$dir/lease.json")" = 600 ] \
    || fail 'handle state is not private'
  client exec "$handle" -- sh -c 'printf token > state' || fail "$protocol first exec failed"
  [ "$(client exec "$handle" -- cat state)" = token ] || fail "$protocol second exec did not see the same VM"
  set +e
  client exec "$handle" -- sh -c 'exit 7'
  rc=$?
  set -e
  [ "$rc" = 7 ] || fail "$protocol exec lost the guest exit status ($rc)"
  printf 'payload\n' | client exec "$handle" --stdin -- sh -c 'cat > stdin-copy'
  [ "$(cat "$TMP/vm/stdin-copy")" = payload ] || fail "$protocol --stdin did not forward input"
  client leases | grep -E "^$handle +slot-002 .* active " >/dev/null || fail "$protocol leases does not show the handle"
  wait_for '[ "$(count renew)" -ge 2 ]' || fail "$protocol keeper did not renew"
  keeper="$(keeper_pid "$handle")"
  # Renewal and release use the route captured at acquire, not a later publication.
  mv "$TMP/routes" "$TMP/routes.away"
  client release "$handle" 2>"$TMP/release.err" || { cat "$TMP/release.err" >&2; fail "$protocol release failed"; }
  mv "$TMP/routes.away" "$TMP/routes"
  [ "$(count release)" = 1 ] || fail "$protocol release did not reach the broker once"
  grep -Fxq 'E2E_RELEASE state=start duration_seconds=0 budget_seconds=180 grace_seconds=5 exit_code=0' "$TMP/release.err" \
    && grep -Eq '^E2E_RELEASE state=end duration_seconds=[0-9]+ budget_seconds=180 grace_seconds=5 exit_code=0$' "$TMP/release.err" \
    || fail "$protocol handle release left no release evidence: $(cat "$TMP/release.err")"
  [ ! -e "$dir" ] || fail "$protocol release kept the handle"
  wait_for '! kill -0 "$keeper" 2>/dev/null' || fail "$protocol keeper outlived its released handle"
  renewals="$(count renew)"
  sleep 2
  [ "$(count renew)" = "$renewals" ] || fail "$protocol keeper renewed after release"
  if client exec "$handle" -- true 2>"$TMP/err"; then fail "$protocol exec succeeded after release"; fi
  grep -Fq "no active lease handle '$handle'" "$TMP/err" || fail "$protocol released handle diagnostic"
  client release "$handle" 2>/dev/null || fail "$protocol repeated release is not idempotent"
done
rm -f "$TMP/v4"

# Keeper death: exec refuses and nothing renews; the broker's expiry ends the lease.
: > "$TMP/facade.log"
handle="$(client acquire --yard test-yard --slot 2 2>/dev/null)"
wait_for '[ "$(count renew)" -ge 1 ]' || fail 'keeper did not start renewing'
keeper="$(keeper_pid "$handle")"
kill -KILL "$keeper"
wait_for '! kill -0 "$keeper" 2>/dev/null' || fail 'killed keeper survived'
renewals="$(count renew)"
if client exec "$handle" -- true 2>"$TMP/err"; then fail 'exec succeeded without a keeper'; fi
grep -Fq 'is not running; the broker expires the lease' "$TMP/err" || fail 'missing keeper-death diagnostic'
client leases | grep -E "^$handle .* no-keeper " >/dev/null || fail 'leases hides the stopped keeper'
sleep 2
[ "$(count renew)" = "$renewals" ] || fail 'a lease renewed after its keeper died'
grep -F 'guest-start' "$TMP/facade.log" >/dev/null && fail 'exec reached the guest without a keeper'
client release "$handle" 2>/dev/null || fail 'release after keeper death failed'

# An orphaned keeper never renews: replacing or removing the handle stops it.
: > "$TMP/facade.log"
handle="$(client acquire --yard test-yard --slot 2 2>/dev/null)"
dir="$TMP/client/handles/$handle"
keeper="$(keeper_pid "$handle")"
cp -p "$dir/lease.json" "$dir/lease.copy"
mv -f "$dir/lease.copy" "$dir/lease.json"
wait_for '! kill -0 "$keeper" 2>/dev/null' || fail 'keeper kept running for a replaced handle'
renewals="$(count renew)"
sleep 2
[ "$(count renew)" = "$renewals" ] || fail 'orphaned keeper renewed'
client release "$handle" 2>/dev/null || fail 'release of a replaced handle failed'

# An in-flight renewal transport dies with its keeper or handle before its request arrives.
for orphan in keeper-killed handle-replaced; do
  : > "$TMP/facade.log"
  touch "$TMP/renew-delay"
  handle="$(client acquire --yard test-yard --slot 2 2>/dev/null)"
  dir="$TMP/client/handles/$handle"
  keeper="$(keeper_pid "$handle")"
  wait_for '[ "$(count connect-renew)" -ge 1 ]' || fail "$orphan: renewal transport did not start"
  if [ "$orphan" = keeper-killed ]; then
    kill -KILL "$keeper"
  else
    cp -p "$dir/lease.json" "$dir/lease.copy"
    mv -f "$dir/lease.copy" "$dir/lease.json"
  fi
  wait_for '! kill -0 "$keeper" 2>/dev/null' || fail "$orphan: keeper survived"
  sleep 4
  [ "$(grep -cx renew "$TMP/facade.log" || true)" = 0 ] \
    || fail "$orphan: an orphaned renewal transport reached the broker"
  rm -f "$TMP/renew-delay"
  client release "$handle" 2>/dev/null || fail "$orphan: release failed"
done

# Renewal transport failures are retried while the lease is safely current; a longer
# outage, unlike a single failure, marks the handle lost.
: > "$TMP/facade.log"
handle="$(client acquire --yard test-yard --slot 2 2>/dev/null)"
touch "$TMP/renew-transport-fails"
wait_for '[ "$(count renew-transport-failure)" -ge 2 ]' || fail 'renewal transport failure was not retried'
[ ! -e "$TMP/client/handles/$handle/lost" ] || fail 'one transport failure marked the lease lost'
rm -f "$TMP/renew-transport-fails"
wait_for '[ "$(grep -cx renew "$TMP/facade.log" || true)" -ge 1 ]' || fail 'renewal did not recover after the transport returned'
[ ! -e "$TMP/client/handles/$handle/lost" ] || fail 'a recovered renewal left the lease lost'
client exec "$handle" -- true || fail 'exec failed after renewal recovered'
touch "$TMP/renew-transport-fails"
wait_for '[ -e "$TMP/client/handles/$handle/lost" ]' || fail 'a long renewal outage did not mark the lease lost'
rm -f "$TMP/renew-transport-fails"
client release "$handle" 2>/dev/null || fail 'release after a renewal outage failed'

# A running exec shows as in use; exec of a trivial command resets the idle timer.
: > "$TMP/facade.log"
handle="$(client acquire --yard test-yard --slot 2 --idle 3s 2>/dev/null)"
client exec "$handle" -- sleep 3 &
exec_pid=$!
wait_for 'grep -q "^guest-start sleep" "$TMP/facade.log"' || fail 'background exec did not start'
client leases | grep -E "^$handle .* active +in-use$" >/dev/null || fail "leases did not show the running exec: $(client leases)"
wait "$exec_pid" || fail 'background exec failed'
sleep 2
client exec "$handle" -- true || fail 'idle reset exec failed'
sleep 2
[ "$(count release)" = 0 ] || fail 'exec HANDLE -- true did not reset the idle timer'
wait_for '[ "$(count release)" = 1 ]' || fail 'idle lease was not released after the reset'

# Repeatedly rejected idle releases stop renewal so broker expiry reclaims the slot.
: > "$TMP/facade.log"
touch "$TMP/release-rejected"
handle="$(client acquire --yard test-yard --slot 2 --idle 1s 2>/dev/null)"
keeper="$(keeper_pid "$handle")"
wait_for '[ -e "$TMP/client/handles/$handle/lost" ]' || fail 'rejected idle releases were retried without bound'
wait_for '! kill -0 "$keeper" 2>/dev/null' || fail 'keeper kept running after abandoning idle release'
[ "$(count release)" = 3 ] || fail "idle release attempts were not bounded at 3: $(count release)"
renewals="$(count renew)"
sleep 2
[ "$(count renew)" = "$renewals" ] || fail 'keeper renewed after abandoning the lease'
rm -f "$TMP/release-rejected"
client release "$handle" 2>/dev/null || fail 'release after abandoned idle release failed'

# Idle limit: the keeper releases an unused lease, but not while an exec runs.
: > "$TMP/facade.log"
handle="$(client acquire --yard test-yard --slot 2 --idle 2s 2>/dev/null)"
client exec "$handle" -- sleep 4
grep -Fq release "$TMP/facade.log" && fail 'idle release interrupted a running exec'
wait_for '[ "$(count release)" = 1 ]' || fail 'idle lease was not released'
wait_for '[ ! -e "$TMP/client/handles/$handle" ]' || fail 'idle release kept the handle'
awk '/^guest-end/ {ended=NR} /^release/ {released=NR} END {exit !(ended && released > ended)}' "$TMP/facade.log" \
  || fail 'idle release did not wait for the running exec'

# A failed renewal marks the lease lost; exec refuses and release clears the handle.
: > "$TMP/facade.log"
handle="$(client acquire --yard test-yard --slot 2 2>/dev/null)"
touch "$TMP/renew-fails"
wait_for '[ -e "$TMP/client/handles/$handle/lost" ]' || fail 'renewal failure was not recorded'
if client exec "$handle" -- true 2>"$TMP/err"; then fail 'exec succeeded on a lost lease'; fi
grep -Fq "lease $handle was lost" "$TMP/err" || fail 'missing lost-lease diagnostic'
client release "$handle" 2>"$TMP/err" || fail 'release of a lost lease failed'
grep -Fq 'had already ended' "$TMP/err" || fail 'release did not report the ended lease'
grep -Eq '^E2E_RELEASE state=end duration_seconds=[0-9]+ budget_seconds=180 grace_seconds=5 exit_code=1$' "$TMP/err" \
  || fail 'an unconfirmed release was recorded as a successful cleanup'
[ ! -e "$TMP/client/handles/$handle" ] || fail 'lost handle was kept after release'
rm -f "$TMP/renew-fails"

# Refusals leave no handle and keep the typed outcome and holder.
for scenario in busy capacity; do
  printf '%s\n' "$scenario" > "$TMP/scenario"
  set +e
  client acquire --yard test-yard --slot 2 >/dev/null 2>"$TMP/err"
  rc=$?
  set -e
  case "$scenario" in
    busy) [ "$rc" = 2 ] && grep -Fq 'state=held reason=busy; owner=other/Other run=run-a purpose=tests' "$TMP/err" \
      || fail "busy refusal: rc=$rc $(cat "$TMP/err")" ;;
    capacity) [ "$rc" = 4 ] && grep -Fq 'retryable capacity refusal for slot-002: resource=memory' "$TMP/err" \
      || fail "capacity refusal: rc=$rc $(cat "$TMP/err")" ;;
  esac
  [ -z "$(ls -A "$TMP/client/handles")" ] || fail "$scenario refusal left a handle"
done
rm -f "$TMP/scenario"

printf 'ok: test-VM client handles span separate commands, fence keepers and release\n'
