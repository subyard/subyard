#!/usr/bin/env bash
# Agent E2E transport copies dirty public inputs, preserves argv and owns only run directories.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
export SUBYARD_E2E_STATE_DIR="$TMP/client"

grep -Fq 'SUBYARD_E2E_ROUTE_REGISTRY:-/var/lib/subyard/e2e-routes' \
  "$ROOT/dev/agent-e2e.sh" \
  || fail 'runner does not use the boot-stable product route registry'
grep -Fq 'target=/var/lib/subyard/e2e-routes' "$ROOT/scripts/03-create-subyard.sh" \
  || fail 'yard route mount and runner registry path diverged'

# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

[ "$E2E_YARD" = test-yard ] || fail "agent runner default yard is not test-yard"
[ "$STATE_ROOT" = "$TMP/client/yards/test-yard" ] \
  || fail "default generated client state is not yard-scoped"
[ "$IDENTITY" = "$TMP/client/id_ed25519" ] \
  || fail "controller identity is not shared outside yard-scoped state"

scope_snapshot="$(
  env -u SUBYARD_E2E_BASTION_ROUTE \
    -u SUBYARD_E2E_STATE_DIR -u SUBYARD_E2E_YARD_STATE_DIR -u SUBYARD_E2E_IDENTITY \
    -u SUBYARD_E2E_YARD SUBYARD_HOME="$TMP/default-client" \
    bash -c '
      set -euo pipefail
      . "$1/dev/agent-e2e.sh"
      printf "%s|%s|%s|%s\n" \
        "$E2E_YARD" "$BASTION_ROUTE" "$STATE_ROOT" "$IDENTITY"
      E2E_YARD=e2e-yard
      configure_yard_scope
      printf "%s|%s|%s|%s\n" \
        "$E2E_YARD" "$BASTION_ROUTE" "$STATE_ROOT" "$IDENTITY"
    ' _ "$ROOT"
)"
expected_scope_snapshot="$(printf '%s\n%s\n' \
  "test-yard|yard-test-yard|$TMP/default-client/e2e/yards/test-yard|$TMP/default-client/e2e/id_ed25519" \
  "e2e-yard|yard-e2e-yard|$TMP/default-client/e2e/yards/e2e-yard|$TMP/default-client/e2e/id_ed25519")"
[ "$scope_snapshot" = "$expected_scope_snapshot" ] \
  || fail "test-yard and explicit e2e-yard route/state scopes collide: $scope_snapshot"
if "$ROOT/dev/agent-e2e.sh" --yard '../unsafe' --prepare >/dev/null 2>&1; then
  fail "agent runner accepted an unsafe yard selector"
fi
if "$ROOT/dev/agent-e2e.sh" --slot 0 --prepare >/dev/null 2>&1; then
  fail "agent runner accepted an invalid exact slot"
fi
LEASE_REQUESTED_SLOT=''
set_requested_slot 1 SUBYARD_P0_SLOT
[ "$LEASE_REQUESTED_SLOT" = slot-001 ] \
  || fail "P0 exact-slot environment did not resolve slot-001"
if (set_requested_slot 0 SUBYARD_P0_SLOT) >/dev/null 2>&1; then
  fail "P0 exact-slot environment accepted slot zero"
fi
LEASE_REQUESTED_SLOT=''

export SUBYARD_E2E_TEST_MODE=1
export SUBYARD_E2E_WORKSPACES_ROOT="$TMP/workspaces"
fixture="$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/src"
mkdir -p "$fixture/private" "$fixture/temp"
git -C "$fixture" init -q
git -C "$fixture" remote add origin \
  'https://token:private-value@github.com/Subyard/Attribution.git?access=private-value'
printf 'private/\ntemp/\nignored.secret\n' > "$fixture/.gitignore"
printf 'tracked\n' > "$fixture/tracked.txt"
printf 'removed\n' > "$fixture/removed.txt"
printf 'dirty\n' > "$fixture/dirty.txt"
printf 'ignored\n' > "$fixture/ignored.secret"
printf 'private\n' > "$fixture/private/note.txt"
printf 'temp\n' > "$fixture/temp/cache.txt"
git -C "$fixture" add .gitignore tracked.txt removed.txt
printf 'changed\n' >> "$fixture/tracked.txt"
rm "$fixture/removed.txt"

fallback_context="$(resolve_workspace_attribution "$fixture")"
[ "$fallback_context" = $'unknown\tSubyard-2-05398f45' ] \
  || fail "safe legacy workspace fallback changed: $fallback_context"
printf '%s\n' \
  '{"schema":1,"projectId":"Subyard-2-05398f45","name":"Subyard-2","yard":"default","mode":"sync"}' \
  > "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json"
[ "$(resolve_workspace_attribution "$fixture")" = $'default\tSubyard-2' ] \
  || fail "runner did not use canonical workspace metadata"
cp "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json" "$TMP/valid-meta"
printf '%s\n' \
  '{"schema":1,"projectId":"foreign","name":"Subyard-2","yard":"default"}' \
  > "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json"
if (resolve_workspace_attribution "$fixture") >/dev/null 2>&1; then
  fail "runner accepted mismatched project metadata"
fi
mv "$TMP/valid-meta" "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json"
run_a="$(new_run_id)"
run_b="$(new_run_id)"
[[ "$run_a" =~ ^[0-9a-f]{8}$ && "$run_b" =~ ^[0-9a-f]{8}$ && "$run_a" != "$run_b" ]] \
  || fail "per-acquire run identities are invalid or reused"
[ "$(derive_purpose run '' bash dev/e2e/release-migration-catch-up.sh auto)" = \
    release-migration-catch-up ] \
  || fail "runner did not derive a bounded script purpose"
[ "$(derive_purpose ssh 'manual diagnostic')" = manual-diagnostic ] \
  || fail "explicit purpose was not normalized"
LEASE_YARD=default
LEASE_PROJECT=Subyard-2
LEASE_CHECKOUT=
LEASE_RUN="$run_a"
LEASE_PURPOSE=contract-tests
LEASE_GENERATION=7
BROKER_ATTRIBUTION_V2=1
lease_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$lease_request" = \
    "acquire-v2 client SHA256:key default Subyard-2 $run_a contract-tests ssh-ed25519 keyblob" ] \
  || fail "runner did not carry canonical attribution through acquire-v2"
LEASE_REQUESTED_SLOT='slot-002'
exact_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$exact_request" = \
  "acquire-v2 client SHA256:key default Subyard-2 $run_a contract-tests ssh-ed25519 keyblob slot-002" ] \
  || fail "runner did not retain the existing exact-slot acquire protocol"
BROKER_ATTRIBUTION_V2=0
LEASE_REQUESTED_SLOT=
legacy_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$legacy_request" = \
  "acquire client SHA256:key Subyard-2+$run_a contract-tests ssh-ed25519 keyblob" ] \
  || fail "old-broker fallback did not use an opaque no-checkout label"
BROKER_ATTRIBUTION_V2=1
LEASE_REQUESTED_SLOT='slot-002'
LEASE_SLOT='slot-002'
lease_grant_matches_request || fail "matching exact-slot grant was rejected"
LEASE_SLOT='slot-001'
if lease_grant_matches_request; then
  fail "mismatched exact-slot grant was accepted before transport"
fi
LEASE_REQUESTED_SLOT=
LEASE_SLOT=

bundle="$TMP/worktree.tar.gz"
build_bundle "$fixture" "$bundle"
contents="$(tar -tzf "$bundle" | sort)"
printf '%s\n' "$contents" | grep -Fxq dirty.txt || fail "dirty untracked file was not copied"
printf '%s\n' "$contents" | grep -Fxq tracked.txt || fail "modified tracked file was not copied"
printf '%s\n' "$contents" | grep -Fxq .subyard-e2e-index \
  || fail "tracked-file inventory was not copied"
! printf '%s\n' "$contents" | grep -Fxq removed.txt || fail "deleted tracked file entered the bundle"
! printf '%s\n' "$contents" | grep -Eq '(^|/)(private|temp|\.git)(/|$)|ignored\.secret' \
  || fail "ignored or private data entered the worktree bundle"
inventory="$(tar -xOf "$bundle" .subyard-e2e-index | tr '\0' '\n')"
printf '%s\n' "$inventory" | grep -Fxq tracked.txt \
  || fail "tracked-file inventory omitted a tracked path"
! printf '%s\n' "$inventory" | grep -Fxq dirty.txt \
  || fail "tracked-file inventory classified an untracked path as tracked"

ln -s /etc/passwd "$fixture/escaping-link"
if (build_bundle "$fixture" "$TMP/unsafe.tar.gz") >/dev/null 2>&1; then
  fail "worktree bundling accepted a symlink outside the repository"
fi
rm "$fixture/escaping-link"

command_root="$TMP/command path"
mkdir -p "$command_root/src"
write_guest_command 2 "$command_root" sh -c \
  'test "$(id -un)" = dev && test "$SUBYARD_E2E_VM" = 2 && test "$1" = "argument with spaces"' \
  fixture 'argument with spaces' \
  > "$TMP/run.sh"
bash -n "$TMP/run.sh" || fail "guest command is not valid shell"
printf -v command_root_q '%q' "$command_root"
grep -Fxq "chown -R dev:dev $command_root_q" "$TMP/run.sh" \
  && grep -Fq 'exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev sh -c' \
    "$TMP/run.sh" \
  && grep -Fq 'fixture argument\ with\ spaces' "$TMP/run.sh" \
  || fail "guest command does not run as dev or preserve its argv"
grep -Fxq 'export SUBYARD_E2E_YARD=test-yard' "$TMP/run.sh" \
  && grep -Fxq 'export SUBYARD_E2E_PROJECT=Subyard-2' "$TMP/run.sh" \
  && grep -Fxq "export SUBYARD_E2E_RUN_ID=$run_a" "$TMP/run.sh" \
  && grep -Fxq 'export SUBYARD_E2E_PURPOSE=contract-tests' "$TMP/run.sh" \
  && grep -Fxq 'export SUBYARD_E2E_GENERATION=7' "$TMP/run.sh" \
  || fail "guest command omitted public lease context"
write_guest_command 1 "$command_root" ./bin/yard --version > "$TMP/yard-run.sh"
grep -Fxq '/usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev ./dev/build-engine.sh' \
  "$TMP/yard-run.sh" \
  || fail "direct guest yard command does not build its explicit development engine"
grep -Fxq 'exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev ./bin/yard --version' \
  "$TMP/yard-run.sh" \
  || fail "direct guest yard command changed its argv after the development build"
quoted="$(quote_ssh_command bash -c 'test "$1" = "argument with spaces"' _ 'argument with spaces')"
bash -c "$quoted" || fail "direct SSH command did not preserve its argv"

normalized_progress="$({
  printf 'download 1%%\rdownload 80%%\rdownload 100%%\n'
  printf 'plain output\n'
  printf 'apt 95%%\rapt 100%%\r\n'
  printf 'final line without newline'
} | normalize_terminal_progress)"
[ "$normalized_progress" = "$(printf '%s\n%s\n%s\n%s' \
  'download 100%' 'plain output' 'apt 100%' 'final line without newline')" ] \
  || fail "runner did not coalesce terminal progress to its final update"
grep -Fq '2>&1 | normalize_terminal_progress' "$ROOT/dev/agent-e2e.sh" \
  || fail "normal guest streams bypass terminal-progress normalization"

mkdir -p "$TMP/direct-bin"
cat > "$TMP/direct-bin/ssh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *' e2e-vm-1 '*)
    IFS= read -r forwarded
    [ "$forwarded" = explicit-stdin ]
    ;;
  *)
    if IFS= read -r leaked; then
      printf 'direct SSH leaked stdin: %s\n' "$leaked" >&2
      exit 91
    fi
    ;;
esac
printf '%s\n' "$*"
SH
chmod +x "$TMP/direct-bin/ssh"
direct_ssh="$(
  PATH="$TMP/direct-bin:$PATH" ROOT="$ROOT" bash -c '
    set -euo pipefail
    . "$ROOT/dev/agent-e2e.sh"
    CLIENT_CONFIG=/tmp/direct-ssh-config
    printf "must-not-reach-ssh\n" | run_direct_ssh 2 0 printf "%s" "argument with spaces"
  '
)"
printf '%s\n' "$direct_ssh" | grep -Fq -- '-T e2e-vm-2 --' \
  || fail "direct SSH command did not use the pinned non-TTY VM route"
direct_ssh_stdin="$(
  PATH="$TMP/direct-bin:$PATH" ROOT="$ROOT" bash -c '
    set -euo pipefail
    . "$ROOT/dev/agent-e2e.sh"
    CLIENT_CONFIG=/tmp/direct-ssh-config
    printf "explicit-stdin\n" | run_direct_ssh 1 1 sh -c "read -r value"
  '
)"
printf '%s\n' "$direct_ssh_stdin" | grep -Fq -- '-T e2e-vm-1 --' \
  || fail "explicit direct SSH stdin did not use the pinned non-TTY VM route"
grep -Fq 'p0_guest "$vm" \' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'dd of="$1" status=none' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 source archive does not use the lease-local stdin transport"
grep -Fq 'set_requested_slot "$SUBYARD_P0_SLOT" SUBYARD_P0_SLOT' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 exact-slot environment does not reach the atomic lease request"
grep -Fq 'run_guest "$vm" "$P0_BUNDLE" "$P0_BUNDLE_HASH" \' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 acceptance repeats the leased runner's dev privilege transition"
grep -Fq 'run_vm "$vm" capacity-preflight' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'capacity-verify-cleanup' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'capacity_report' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 acceptance does not enforce capacity preflight, peak reporting and exact cleanup"
grep -Fq 'P0_E2E_MIN_PEAK_MEMORY_RESERVE_BYTES:-268435456' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 acceptance does not preserve a 256 MiB minimum peak memory reserve"
grep -Fq 'first_unreachable_unix' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'collect_failure_diagnostics' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'lease_keeper_last' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 acceptance does not persist transport, capacity and lease failure evidence"
grep -Fq 'P0_NESTED_VM="${SUBYARD_P0_NESTED_VM:-1}"' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'case "$P0_NESTED_VM" in' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_vm "$P0_NESTED_VM" nested-teardown' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 nested teardown repeats a host-boundary fixture across one shared allocation"
grep -Fq 'run_phase capacity-report targeted_capacity_report' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  && [ "$(grep -Fc '    start_capacity_monitors' \
    "$ROOT/dev/e2e/p0-acceptance.sh")" -eq 2 ] \
  || fail "targeted nested teardown does not monitor both allocated VMs through cleanup"
grep -Fq 'assert_capacity_transport_stable' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "targeted nested teardown can pass after losing an allocated VM"
grep -Fq 'capacity_sample_command="$(quote_ssh_command bash -c' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 capacity monitor loses its remote bash command at the OpenSSH argv boundary"
grep -Fq 'collect_failure_diagnostics failure-entry truncate' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'collect_failure_diagnostics post-stop append' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 failure evidence cannot distinguish failure-entry state from post-stop state"
grep -Fq 'LIMITS_MEMORY=2GiB' "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  && grep -Fq 'NESTED_TEARDOWN_VM_MEMORY_BYTES:-2147483648' \
    "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  && grep -Fq 'NESTED_TEARDOWN_POST_LAUNCH_RESERVE_BYTES:-1073741824' \
    "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  || fail "nested teardown fixture can exhaust its 4 GiB allocated host"
memory_reserve_source="$(awk '
  /^(nested_decimal_at_most|nested_monotonic_seconds|nested_memory_available_bytes|require_nested_memory_reserve)\(\)/ {
    copying=1
  }
  copying { print }
  copying && /^}$/ { copying=0 }
' "$ROOT/dev/e2e/nested-teardown-data-boundary.sh")"
memory_sample_count="$TMP/nested-memory-sample-count"
memory_clock="$TMP/nested-memory-clock"
memory_sleep_log="$TMP/nested-memory-sleep-log"
printf '0\n' > "$memory_sample_count"
printf '0\n' > "$memory_clock"
: > "$memory_sleep_log"
set +e
memory_wait_output="$(
  MEMORY_SAMPLE_COUNT="$memory_sample_count" MEMORY_CLOCK="$memory_clock" \
    MEMORY_SLEEP_LOG="$memory_sleep_log" bash -c "
$memory_reserve_source
die() { printf 'fixture failure: %s\\n' \"\$*\" >&2; exit 2; }
nested_monotonic_seconds() { cat \"\$MEMORY_CLOCK\"; }
nested_memory_available_bytes() {
  count=\"\$(cat \"\$MEMORY_SAMPLE_COUNT\")\"
  count=\"\$((count + 1))\"
  printf '%s\\n' \"\$count\" > \"\$MEMORY_SAMPLE_COUNT\"
  if [ \"\$count\" -eq 1 ]; then
    printf '3000000000\\n'
  else
    printf '3300000000\\n'
  fi
}
sleep() {
  printf '%s\\n' \"\$1\" >> \"\$MEMORY_SLEEP_LOG\"
  now=\"\$(cat \"\$MEMORY_CLOCK\")\"
  printf '%s\\n' \"\$((now + \$1))\" > \"\$MEMORY_CLOCK\"
}
NESTED_TEARDOWN_MEMORY_WAIT_SECONDS=5
NESTED_TEARDOWN_MEMORY_POLL_SECONDS=1
require_nested_memory_reserve
" 2>&1
)"
memory_wait_rc=$?
set -e
[ "$memory_wait_rc" = 0 ] && [ "$(cat "$memory_sample_count")" = 2 ] \
  && [ "$(cat "$memory_sleep_log")" = 1 ] \
  || fail "nested teardown did not wait for transient memory pressure: rc=$memory_wait_rc output=$memory_wait_output"
printf '0\n' > "$memory_sample_count"
printf '0\n' > "$memory_clock"
: > "$memory_sleep_log"
set +e
memory_deadline_output="$(
  MEMORY_SAMPLE_COUNT="$memory_sample_count" MEMORY_CLOCK="$memory_clock" \
    MEMORY_SLEEP_LOG="$memory_sleep_log" bash -c "
$memory_reserve_source
die() { printf 'fixture failure: %s\\n' \"\$*\" >&2; exit 2; }
nested_monotonic_seconds() { cat \"\$MEMORY_CLOCK\"; }
nested_memory_available_bytes() {
  count=\"\$(cat \"\$MEMORY_SAMPLE_COUNT\")\"
  printf '%s\\n' \"\$((count + 1))\" > \"\$MEMORY_SAMPLE_COUNT\"
  printf '3000000000\\n'
}
sleep() {
  printf '%s\\n' \"\$1\" >> \"\$MEMORY_SLEEP_LOG\"
  now=\"\$(cat \"\$MEMORY_CLOCK\")\"
  printf '%s\\n' \"\$((now + \$1))\" > \"\$MEMORY_CLOCK\"
}
NESTED_TEARDOWN_MEMORY_WAIT_SECONDS=5
NESTED_TEARDOWN_MEMORY_POLL_SECONDS=3
require_nested_memory_reserve
" 2>&1
)"
memory_deadline_rc=$?
set -e
[ "$memory_deadline_rc" = 2 ] && [ "$(cat "$memory_sample_count")" = 3 ] \
  && [ "$(cat "$memory_sleep_log")" = $'3\n2' ] \
  && grep -Fq 'after waiting 5s' <<<"$memory_deadline_output" \
  || fail "nested teardown memory wait exceeded its deadline: rc=$memory_deadline_rc output=$memory_deadline_output"
set +e
memory_overflow_output="$(bash -c "
$memory_reserve_source
die() { printf 'fixture failure: %s\\n' \"\$*\" >&2; exit 2; }
nested_monotonic_seconds() { printf '0\\n'; }
nested_memory_available_bytes() { printf '1\\n'; }
sleep() { exit 91; }
NESTED_TEARDOWN_VM_MEMORY_BYTES=9223372036854775807
NESTED_TEARDOWN_POST_LAUNCH_RESERVE_BYTES=1
NESTED_TEARDOWN_MEMORY_WAIT_SECONDS=0
require_nested_memory_reserve
" 2>&1)"
memory_overflow_rc=$?
set -e
[ "$memory_overflow_rc" = 2 ] \
  && grep -Fq 'nested memory requirement exceeds the supported range' \
    <<<"$memory_overflow_output" \
  || fail "nested teardown memory arithmetic overflowed fail-open: rc=$memory_overflow_rc output=$memory_overflow_output"
grep -Fq 'NESTED_TEARDOWN_COMMAND_TIMEOUT:-2700' \
  "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  && grep -Fq 'NESTED_TEARDOWN_COMMAND_KILL_AFTER_SECONDS:-10' \
    "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  && grep -Fq 'timeout --signal=TERM --kill-after="$COMMAND_KILL_AFTER_SECONDS"' \
    "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  && ! grep -Fq 'timeout --foreground "$COMMAND_TIMEOUT"' \
    "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  || fail "nested teardown clean-init commands are not hard-bounded for a loaded full matrix"
bounded_command_source="$(awk '
  /^bounded_command\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/nested-teardown-data-boundary.sh")"
set +e
bounded_failure="$(
  COMMAND_TIMEOUT=5 COMMAND_KILL_AFTER_SECONDS=1 bash -c \
    "$bounded_command_source
bounded_command injected bash -c 'exit 23'" 2>&1
)"
bounded_failure_rc=$?
set -e
[ "$bounded_failure_rc" = 23 ] && [ -z "$bounded_failure" ] \
  || fail "nested teardown bounded command masked exit 23: rc=$bounded_failure_rc output=$bounded_failure"
set +e
bounded_timeout="$(
  COMMAND_TIMEOUT=1 COMMAND_KILL_AFTER_SECONDS=1 bash -c \
    "$bounded_command_source
bounded_command injected sleep 5" 2>&1
)"
bounded_timeout_rc=$?
set -e
[ "$bounded_timeout_rc" = 124 ] \
  && grep -Fq 'injected exceeded the 1s command deadline' <<<"$bounded_timeout" \
  || fail "nested teardown bounded command masked its deadline: rc=$bounded_timeout_rc output=$bounded_timeout"
lane_inventory="$("$ROOT/dev/e2e/p0-acceptance.sh" --list-lanes)"
for lane in boundary nested-teardown transport dependencies real-incus profile-resource release source-upgrade \
  power-systemd \
  reboot-verify peer peer-cleanup cleanup; do
  grep -qx "$lane" <<<"$lane_inventory" || fail "P0 lane inventory omitted $lane"
done
lane_table="$(sed -n '/^| Lane |/,/^$/p' "$ROOT/docs/test-vms.md")"
while IFS= read -r lane; do
  case "$lane" in
    full$'\t'*) continue ;;
  esac
  grep -Fq "\`$lane\`" <<<"$lane_table" \
    || fail "public P0 lane table omitted $lane"
done <<<"$lane_inventory"
grep -Fq $'full\tboundary transport nested-teardown release source-upgrade power-systemd peer cleanup' <<<"$lane_inventory" \
  || fail 'continuous P0 gate lost a mandatory public-contract phase'
grep -Fq 'full_parallel_matrix()' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_vm 1 owner' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_full_aux_stage nested-teardown run_vm 2 nested-teardown' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_full_aux_stage controller run_vm 2 controller' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_full_aux_stage source-upgrade source_upgrade_lane 2' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_full_aux_stage power-systemd power_systemd_lane 2' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'continuous P0 serializes its bounded VM2 fixtures behind the long VM1 owner lane'
grep -Fq 'mark_full_matrix_passed()' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq '.lanes["nested-teardown"] = "passed"' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq '.lanes["release"] = "passed"' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq '.lanes["source-upgrade"] = "passed"' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq '.lanes["power-systemd"] = "passed"' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'continuous P0 can checkpoint only part of its parallel release matrix'
grep -Fq 'arm_full_fixture "$FULL_SOURCE_ARM_FILE" 2' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'arm_full_fixture "$FULL_POWER_ARM_FILE" 2' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'cleanup_armed_full_fixtures' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'prepare_source_archive 2' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_full_aux_stage fixture-platform run_vm 2 real-incus' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'FULL_MATRIX_TIMEOUT_SECONDS="${SUBYARD_P0_FULL_MATRIX_TIMEOUT_SECONDS:-12600}"' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'continuous P0 parallel fixtures are not cleanup-armed, bootstrapped or bounded to 210 minutes'
grep -Fq 'RUNNER_STOP_GRACE_SECONDS=30' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'RUNNER_KILL_GRACE_SECONDS=10' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'p0_monotonic_seconds()' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'read -r uptime _ < /proc/uptime' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && ! grep -Fq '$SECONDS' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'start_runner_child()' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'set -m' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'kill -TERM -- "-$root"' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'kill -KILL -- "-$root"' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'stop_deadline=$((now + RUNNER_STOP_GRACE_SECONDS))' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'kill_deadline=$((now + RUNNER_KILL_GRACE_SECONDS))' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'continuous P0 deadline is not monotonic or does not bound TERM/KILL child shutdown'
(
  eval "$(sed -n '/^p0_monotonic_seconds() {/,/^}/p' "$ROOT/dev/e2e/p0-acceptance.sh")"
  before="$(p0_monotonic_seconds)"
  SECONDS=$((SECONDS + 30000))
  after="$(p0_monotonic_seconds)"
  [ "$after" -ge "$before" ] && [ "$((after - before))" -lt 5 ] \
    || fail 'P0 monotonic clock followed an artificial Bash SECONDS jump'
)
(
  runner_pid=''
  trap '
    [ -z "$runner_pid" ] || kill -KILL -- "-$runner_pid" >/dev/null 2>&1 || true
  ' EXIT
  eval "$(sed -n '/^start_runner_child() {/,/^}/p' "$ROOT/dev/e2e/p0-acceptance.sh")"
  eval "$(sed -n '/^stop_runner_children() {/,/^}/p' "$ROOT/dev/e2e/p0-acceptance.sh")"
  eval "$(sed -n '/^p0_monotonic_seconds() {/,/^}/p' "$ROOT/dev/e2e/p0-acceptance.sh")"
  # shellcheck disable=SC2034
  RUNNER_STOP_GRACE_SECONDS=1
  # shellcheck disable=SC2034
  RUNNER_KILL_GRACE_SECONDS=1
  start_runner_child bash -c '
    trap "" TERM
    (sleep 0.2; trap "" TERM; while :; do sleep 10; done) &
    while :; do sleep 10; done
  '
  runner_pid="$P0_STARTED_PID"
  started=$SECONDS
  stop_runner_children 2>/dev/null
  [ "$((SECONDS - started))" -le 4 ] \
    || fail 'runner child shutdown exceeded its TERM/KILL grace'
  ! kill -0 -- "-$runner_pid" >/dev/null 2>&1 \
    || fail 'runner process group survived bounded TERM/KILL shutdown'
  runner_pid=''
)
[ -r "$ROOT/dev/e2e/lib-p0-init-retry.sh" ] \
  || fail 'P0 owner lane has no bounded stale-init retry helper'
# shellcheck source=dev/e2e/lib-p0-init-retry.sh
. "$ROOT/dev/e2e/lib-p0-init-retry.sh"
retry_count="$TMP/init-retry-count"
stale_once_then_succeed() {
  local count=0
  [ ! -r "$retry_count" ] || count="$(cat "$retry_count")"
  count=$((count + 1))
  printf '%s\n' "$count" > "$retry_count"
  [ "$count" -gt 1 ] || {
    printf '%s\n' \
      'yard: init: operation plan is stale: action consequences changed after confirmation' >&2
    return 1
  }
}
P0_INIT_STALE_RETRY_DELAY_SECONDS=0 \
  p0_retry_init_after_plan_stale stale_once_then_succeed \
  >"$TMP/init-retry.log" 2>&1 \
  || fail 'P0 stale-init retry did not accept a freshly reassessed plan'
[ "$(cat "$retry_count")" = 2 ] \
  && grep -Fq 'retrying with a fresh plan (2/3)' "$TMP/init-retry.log" \
  || fail 'P0 stale-init retry did not perform exactly one visible retry'
printf '0\n' > "$retry_count"
unrelated_init_failure() {
  local count
  count="$(cat "$retry_count")"
  printf '%s\n' "$((count + 1))" > "$retry_count"
  printf 'yard: init: unrelated failure\n' >&2
  return 1
}
set +e
P0_INIT_STALE_RETRY_DELAY_SECONDS=0 \
  p0_retry_init_after_plan_stale unrelated_init_failure \
  >"$TMP/init-unrelated.log" 2>&1
unrelated_rc=$?
set -e
[ "$unrelated_rc" = 1 ] && [ "$(cat "$retry_count")" = 1 ] \
  || fail 'P0 stale-init retry masked or repeated an unrelated failure'
for invalid_delay in 00 61 18446744073709551616; do
  set +e
  P0_INIT_STALE_RETRY_DELAY_SECONDS="$invalid_delay" \
    p0_retry_init_after_plan_stale true >/dev/null 2>&1
  invalid_delay_rc=$?
  set -e
  [ "$invalid_delay_rc" = 2 ] \
    || fail "P0 stale-init retry accepted unbounded retry delay $invalid_delay"
done
grep -Fq 'p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'P0 owner legacy convergence bypasses the bounded stale-plan retry'
[ "$(grep -Fc 'p0_retry_init_after_plan_stale "$old_yard" -Y test-yard init --yes' \
  "$ROOT/dev/e2e/p0-guest.sh")" -eq 2 ] \
  && [ "$(grep -Fc 'p0_retry_init_after_plan_stale "$old_yard" -Y e2e-yard init --yes' \
    "$ROOT/dev/e2e/p0-guest.sh")" -eq 1 ] \
  || fail 'P0 owner release fixtures bypass the bounded stale-plan retry'
for worker_fixture in p0-source-upgrade.sh power-reconciler-systemd-255.sh \
  power-reconciler-systemd.sh power-reconciler-upgrade.sh; do
  grep -Fq 'case "${SUBYARD_E2E_VM:-}" in' "$ROOT/dev/e2e/$worker_fixture" \
    && grep -Fq '1|2)' "$ROOT/dev/e2e/$worker_fixture" \
    || fail "$worker_fixture cannot run on the allocated full-matrix worker VM"
done
! grep -Fq 'if [ "$SUBYARD_E2E_VM" = 1 ]; then' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'P0 cleanup skips source-upgrade and power-systemd residue assertions on VM2'
grep -Fq 'run_phase power-systemd-platform run_vm 1 real-incus' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'targeted power-systemd lane does not prepare the Incus platform and image cache'
grep -Fq 'run_phase source-upgrade-platform run_vm 1 real-incus' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'targeted source-upgrade lane does not prepare the Incus platform and image cache'
grep -Fq 'WAIT_SECONDS="${SUBYARD_P0_WAIT_SECONDS:-0}"' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'P0 acceptance cannot wait atomically for shared broker capacity'
grep -Fq '.allocation == {slot: $slot, resource_generation: $generation}' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq '.bundle_hash == $bundle' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq "die 'checkpoint does not match this allocation generation and exact bundle hash'" \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'P0 resume checkpoint does not fail closed on allocation or bundle drift'
grep -Fq 'P0_CURRENT_PHASE=final-verify' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_phase cleanup cleanup_lane' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && [ "$(grep -c '^    verify_boundary$' "$ROOT/dev/e2e/p0-acceptance.sh")" -eq 1 ] \
  || fail 'continuous P0 can skip final cleanup or boundary verification'
grep -Fq 'prepare_slot "$slot"' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'LEASE_PURPOSE=acceptance-prepare' \
    "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'acquire_lease' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'guest "$vm" sh -eu -c' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'fstrim -av' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'dump_broker_diagnostics' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail "P1 acceptance does not prepare first-boot slots sequentially with diagnostics"
grep -Fq 'systemctl is-enabled --quiet subyard-e2e-lease-context.service' \
  "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq '/proc/sys/kernel/random/boot_id' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq '/run/subyard-e2e-lease.json' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail 'lease acceptance does not verify context restoration after a holder reboot'
grep -Fq 'go build -cover' "$ROOT/dev/process-coverage.sh" \
  && grep -Fq 'go tool covdata merge' "$ROOT/dev/process-coverage.sh" \
  && grep -Fq 'SUBYARD_SHELL_COVERAGE_LOG' "$ROOT/dev/process-coverage.sh" \
  && grep -Fq 'bundle_hash' "$ROOT/dev/process-coverage.sh" \
  || fail 'process coverage does not merge an instrumented yard with Shell inventory evidence'
grep -Fq 'reclaim_owner_lease_capacity' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'owner lease fixture pool reserve' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'p0_capacity_reclaim_go_module_cache' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'OWNER_BASELINE_IMAGES' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not reclaim only test-owned migration capacity"
grep -Fq '/tmp/subyard-hermes-profile.*/storage' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'recover_existing_p0=0' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq '[ "$token" != "$P0_CAPACITY_TOKEN" ] || return 0' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'stale P0 pool still has an active process' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq '[ "$used_by" = '\''[]'\'' ]' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'P0 preflight does not narrowly recover an unused stale test pool'
grep -Fq 'is_markerless_migrated_owner_project' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq '"$stale_root/owner/config/yards/test-yard.env"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'grep -Fxc -- "# $expected_marker" "$registration"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'user.subyard.test_vms_revision' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq '1:*:test-yard' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'incus project set subyard-test-yard user.subyard.p0-image-cache="$MARKER"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'P0 owner migration does not fence and restore its transient project marker'
grep -Fq 'recover_stale_source_upgrade_fixture' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'p0_source_fixture_cleanup_token "$token"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'source_upgrade_fixture_active "$token"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'source-upgrade fixture still has an active process' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'timeout --foreground "$query_timeout" pgrep -f -- "$pattern"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'timeout --signal=TERM --kill-after="$kill_after"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -A3 -F 'capacity_preflight()' "$ROOT/dev/e2e/p0-guest.sh" \
    | grep -Fq 'recover_stale_source_upgrade_fixture' \
  || fail 'P0 preflight cannot recover a marker-owned interrupted source-upgrade fixture'
source_recovery_function="$(awk '
  /^recover_stale_source_upgrade_fixture\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/p0-guest.sh")"
run_source_recovery_case() {
  local inventory="$1" e2e_marker="$2" test_marker="$3" default_marker="$4"
  local fixture_active="${5:-0}"
  SOURCE_RECOVERY_INVENTORY="$inventory" \
  SOURCE_RECOVERY_E2E_MARKER="$e2e_marker" \
  SOURCE_RECOVERY_TEST_MARKER="$test_marker" \
  SOURCE_RECOVERY_DEFAULT_MARKER="$default_marker" \
  SOURCE_RECOVERY_FIXTURE_ACTIVE="$fixture_active" \
  SOURCE_RECOVERY_CLEANUP_LOG="$SOURCE_RECOVERY_CLEANUP_LOG" \
  SOURCE_RECOVERY_FUNCTION="$source_recovery_function" bash -c '
    set -euo pipefail
    P0_CAPACITY_TOKEN=999
    SUBYARD_E2E_VM=2
    cleanup_token=
    die() { printf "%s\n" "$*" >&2; return 2; }
    incus() { :; }
    source_upgrade_project_inventory() { printf "%s\n" "$SOURCE_RECOVERY_INVENTORY"; }
    source_upgrade_project_marker() {
      case "$1" in
        subyard-e2e-yard) printf "%s\n" "$SOURCE_RECOVERY_E2E_MARKER" ;;
        subyard-test-yard) printf "%s\n" "$SOURCE_RECOVERY_TEST_MARKER" ;;
        subyard) printf "%s\n" "$SOURCE_RECOVERY_DEFAULT_MARKER" ;;
        *) return 3 ;;
      esac
    }
    source_upgrade_fixture_active() {
      case "$SOURCE_RECOVERY_FIXTURE_ACTIVE" in
        0) return 1 ;;
        1) return 0 ;;
        *) return 2 ;;
      esac
    }
    p0_source_fixture_cleanup_token() {
      cleanup_token="$1"
      printf "%s\n" "$1" >> "$SOURCE_RECOVERY_CLEANUP_LOG"
    }
    eval "$SOURCE_RECOVERY_FUNCTION"
    recover_stale_source_upgrade_fixture
    printf "%s\n" "$cleanup_token"
  '
}
SOURCE_RECOVERY_CLEANUP_LOG="$TMP/source-recovery-cleanup.log"
export SOURCE_RECOVERY_CLEANUP_LOG
source_recovery_result="$(run_source_recovery_case $'subyard-test-yard\nsubyard' '' \
  subyard-p0-source-441 subyard-p0-source-441)"
[ "$(tail -n 1 <<<"$source_recovery_result")" = 441 ] \
  || fail 'P0 source fixture recovery did not select its exact stale token'
[ -z "$(run_source_recovery_case subyard-test-yard '' '' '')" ] \
  || fail 'P0 source fixture recovery mutated an unmarked project'
: > "$SOURCE_RECOVERY_CLEANUP_LOG"
set +e
source_recovery_failure="$(run_source_recovery_case \
  subyard-test-yard '' subyard-p0-source-441 '' 1 2>&1)"
source_recovery_rc=$?
set -e
[ "$source_recovery_rc" = 2 ] \
  && grep -Fq 'active process' <<<"$source_recovery_failure" \
  && [ ! -s "$SOURCE_RECOVERY_CLEANUP_LOG" ] \
  || fail 'P0 source fixture recovery cleaned an active fixture'
: > "$SOURCE_RECOVERY_CLEANUP_LOG"
set +e
source_recovery_failure="$(run_source_recovery_case \
  subyard-test-yard '' subyard-p0-source-441 '' error 2>&1)"
source_recovery_rc=$?
set -e
[ "$source_recovery_rc" = 2 ] \
  && grep -Fq 'cannot determine whether' <<<"$source_recovery_failure" \
  && [ ! -s "$SOURCE_RECOVERY_CLEANUP_LOG" ] \
  || fail 'P0 source fixture recovery treated a liveness query error as stale'
for unsafe_source_markers in foreign-marker conflicting-markers malformed-marker; do
  set +e
  case "$unsafe_source_markers" in
    foreign-marker)
      source_recovery_failure="$(run_source_recovery_case subyard-test-yard '' foreign '' 2>&1)"
      ;;
    conflicting-markers)
      source_recovery_failure="$(run_source_recovery_case \
        $'subyard-test-yard\nsubyard' '' subyard-p0-source-441 \
        subyard-p0-source-442 2>&1)"
      ;;
    malformed-marker)
      source_recovery_failure="$(run_source_recovery_case \
        subyard-test-yard '' subyard-p0-source-44x '' 2>&1)"
      ;;
  esac
  source_recovery_rc=$?
  set -e
  [ "$source_recovery_rc" = 2 ] \
    && grep -Fq 'refusing' <<<"$source_recovery_failure" \
    || fail "P0 source fixture recovery accepted $unsafe_source_markers"
done
grep -Fq 'cold Go dependency download heartbeat elapsed=' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'dependency download failed (attempt %s/3); retrying in %ss' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'timeout --signal=TERM --kill-after=10' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'dependency bootstrap lacks bounded retry and heartbeat progress'
grep -Fq 'Incus default-pool query exceeded' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'timeout --foreground "$query_timeout"' \
    "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  || fail 'P0 capacity preflight can hang on a partial Incus daemon'
grep -Fq 'p0_capacity_recover_stale_roots' "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  && grep -Fq 'stale P0 state still has an active process' \
    "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  || fail 'P0 preflight cannot distinguish orphaned from active marker-owned cache state'
grep -Fq 'OWNER_DIAGNOSTIC_DEV_UID="${P0_E2E_DIAGNOSTIC_DEV_UID:-1001}"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'chown -R "$OWNER_DIAGNOSTIC_DEV_UID:$OWNER_DIAGNOSTIC_DEV_UID" "$bound"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 bind fixture is not owned by its configured diagnostic yard UID"
grep -Fq 'systemctl start subyard-test-vms-host-sink.service' \
  "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'test-vms-broker-incidents' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail "P1 diagnostics do not flush and print the immutable broker incident"
grep -Fq 'FAULT_ROOT=/run/subyard-p0-incus-fault' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'RECOVERY_POLL_SECONDS=2' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'P0_BROKER_RECOVERY_WAIT_SECONDS:-6000' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq '[ "$RECOVERY_WAIT_SECONDS" -le 7200 ]' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'P0_BROKER_RECOVERY_STATUS_TIMEOUT_SECONDS:-30' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'timeout --signal=TERM --kill-after="$RECOVERY_STATUS_KILL_AFTER_SECONDS"' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'sleep "$sleep_seconds"' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'wait_for_slot_state slot-001 available "$RECOVERY_WAIT_SECONDS"' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'reclaim_held_pair_capacity "$VICTIM_CONFIG" victim' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'reclaim_held_pair_capacity "$NEIGHBOR_CONFIG" neighbor' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'outer_root systemctl mask --runtime --now \' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && awk '
    /^stop_slot_pair 2$/ { stopped = NR }
    /^rollback_candidate_update$/ { rolled_back = NR }
    /outer_root systemctl unmask --runtime/ { unmasked = NR }
    END {
      exit !(stopped && rolled_back && unmasked &&
        stopped < rolled_back && rolled_back < unmasked)
    }
  ' "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  || fail "P0 broker recovery does not isolate its targeted fault before rebuilding"
recovery_wait_source="$(awk '
  /^wait_for_slot_state\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/p0-broker-recovery.sh")"
recovery_clock="$TMP/recovery-clock"
recovery_status_budgets="$TMP/recovery-status-budgets"
recovery_sleep_log="$TMP/recovery-sleep-log"
printf '0\n' > "$recovery_clock"
: > "$recovery_status_budgets"
: > "$recovery_sleep_log"
set +e
recovery_wait_output="$({
  RECOVERY_WAIT_SOURCE="$recovery_wait_source" \
    RECOVERY_CLOCK="$recovery_clock" \
    RECOVERY_STATUS_BUDGETS="$recovery_status_budgets" \
    RECOVERY_SLEEP_LOG="$recovery_sleep_log" bash -c '
      set -eu
      eval "$RECOVERY_WAIT_SOURCE"
      RECOVERY_POLL_SECONDS=2
      RECOVERY_STATUS_TIMEOUT_SECONDS=30
      RECOVERY_STATUS_KILL_AFTER_SECONDS=10
      recovery_monotonic_seconds() { cat "$RECOVERY_CLOCK"; }
      status() {
        request_timeout=$1
        printf "%s\n" "$request_timeout" >> "$RECOVERY_STATUS_BUDGETS"
        now="$(cat "$RECOVERY_CLOCK")"
        printf "%s\n" "$((now + request_timeout + RECOVERY_STATUS_KILL_AFTER_SECONDS))" \
          > "$RECOVERY_CLOCK"
        return 124
      }
      sleep() {
        printf "%s\n" "$1" >> "$RECOVERY_SLEEP_LOG"
        now="$(cat "$RECOVERY_CLOCK")"
        printf "%s\n" "$((now + $1))" > "$RECOVERY_CLOCK"
      }
      wait_for_slot_state slot-001 available 65
    '
} 2>&1)"
recovery_wait_rc=$?
set -e
[ "$recovery_wait_rc" = 1 ] \
  && [ "$(cat "$recovery_clock")" = 65 ] \
  && [ "$(cat "$recovery_status_budgets")" = $'30\n13' ] \
  && [ "$(cat "$recovery_sleep_log")" = 2 ] \
  || fail "P0 broker recovery wait exceeded its wall-clock deadline: rc=$recovery_wait_rc output=$recovery_wait_output"
if grep -Fq 'mask --runtime --now incus.service' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh"; then
  fail "P0 broker recovery fault injection drains unrelated held leases"
fi
grep -Fq '> "$PEER_ROOT/config/config.env"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'P0_PEER_YARD_TIMEOUT:-1800' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer yard does not use its active config root with a bounded init"
grep -Fq '"$candidate_yard" _migrate finalize' \
  "$ROOT/scripts/migrate-source-install.sh" \
  && ! grep -Fq '_migrate-test-yard' "$ROOT/dev/bootstrap-runtime.sh" \
  || fail "source bootstrap does not use the generic ordered migration lifecycle"

ensure_identity
lease_blob="$(awk '{print $2}' "$IDENTITY.pub")"
lease_response="$(printf '{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-001","resource_generation":5,"lease_id":"aabb","capability":"ccdd","lease_epoch":3,"data_user":"subyard-e2e-slot-1","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.1.11","host_key_type":"ssh-ed25519","host_key_blob":"%s"},{"selector":2,"name":"e2e-vm-2","address":"10.42.1.12","host_key_type":"ssh-ed25519","host_key_blob":"%s"}]}}' "$lease_blob" "$lease_blob")"
parse_lease_grant "$lease_response" \
  || fail "valid lease grant was rejected"
[ "$LEASE_SLOT" = slot-001 ] && [ "$DATA_USER" = subyard-e2e-slot-1 ] \
  && [ "$LEASE_GENERATION" = 5 ] \
  && [ "${VM_IP[1]}" = 10.42.1.11 ] && [ "${VM_IP[2]}" = 10.42.1.12 ] \
  || fail "lease grant did not materialize exact fenced transport state"
if (parse_lease_grant '{"status":"ok","grant":{"capability":"secret"}}') >/dev/null 2>&1; then
  fail "incomplete lease grant was accepted"
fi
LEASE_PROJECT=Subyard/Attribution
LEASE_CHECKOUT='checkout-a'
LEASE_RUN='run-a'
LEASE_PURPOSE=contract-tests
structured_response="$(printf '{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-002","lease_id":"eeff","capability":"1122","lease_epoch":4,"context":{"schema_version":1,"project":"Subyard/Attribution","checkout":"checkout-a","run":"run-a","purpose":"contract-tests"},"data_user":"subyard-e2e-slot-2","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.2.11","host_key_type":"ssh-ed25519","host_key_blob":"%s"},{"selector":2,"name":"e2e-vm-2","address":"10.42.2.12","host_key_type":"ssh-ed25519","host_key_blob":"%s"}]}}' "$lease_blob" "$lease_blob")"
parse_lease_grant "$structured_response" \
  || fail "structured lease grant was rejected"
[ "$LEASE_SLOT" = slot-002 ] \
  || fail "structured lease context was lost"
changed_context="$(jq -c '.grant.context.project = "Foreign/Project"' <<<"$structured_response")"
if (parse_lease_grant "$changed_context") >/dev/null 2>&1; then
  fail "facade attribution mismatch was accepted"
fi
ensure_identity
BASTION_HOSTNAME=127.0.0.1
BASTION_PORT=2223
BASTION_HOST_KEY_ALIAS=''
BASTION_KNOWN_HOSTS="$TMP/bastion-known-hosts"
DATA_USER=subyard-e2e-slot-1
GUEST_IDENTITY="$TMP/lease-key"
cp "$IDENTITY" "$GUEST_IDENTITY"
printf '[127.0.0.1]:2223 %s\n' "$(normalized_public_key_file "$IDENTITY.pub")" > "$BASTION_KNOWN_HOSTS"
write_client_config
grep -Fxq '    ProxyJump subyard-e2e-data' "$CLIENT_CONFIG" \
  && grep -Fxq '    User subyard-e2e-slot-1' "$CLIENT_CONFIG" \
  || fail "VM aliases do not use the lease-scoped data account"
grep -Fxq '    ForwardAgent no' "$CLIENT_CONFIG" \
  || fail "generated SSH config permits agent forwarding"
[ "$(grep -c '^Host e2e-vm-' "$CLIENT_CONFIG")" -eq 2 ] \
  || fail "generated SSH config does not expose exactly two VM aliases"
[ "$(grep '^[[:space:]]*IdentityFile ' "$CLIENT_CONFIG" | sort -u | wc -l)" -eq 2 ] \
  || fail "controller and ephemeral guest identities were not separated"

cat > "$TMP/route-config" <<EOF
Host fixture-e2e-yard
    HostName 127.0.0.1
    Port 2223
    UserKnownHostsFile $BASTION_KNOWN_HOSTS
EOF
# shellcheck disable=SC2100 # This is an SSH host alias, not an arithmetic expression.
BASTION_ROUTE=fixture-e2e-yard
BASTION_HOSTNAME=''; BASTION_PORT=''; BASTION_HOST_KEY_ALIAS=''; BASTION_KNOWN_HOSTS=''
SUBYARD_E2E_ROUTE_CONFIG="$TMP/route-config"
SUBYARD_E2E_ROUTE_REGISTRY="$TMP/empty-route-registry"
resolve_bastion_route
[ "$BASTION_HOSTNAME:$BASTION_PORT" = 127.0.0.1:2223 ] \
  || fail "bastion route was not resolved from the isolated user SSH config"
[ "$BASTION_KNOWN_HOSTS" = "$TMP/bastion-known-hosts" ] \
  || fail "bastion route did not reuse its pre-pinned host key"

route_registry="$TMP/route-registry"
mkdir -p "$route_registry/test-yard/.route-fixture"
ln -s .route-fixture "$route_registry/test-yard/current"
cat > "$route_registry/test-yard/current/route.tsv" <<'EOF'
subyard-e2e-route-v1
hostname	10.24.0.8
port	22
host_key_alias	subyard-e2e-bastion
EOF
printf 'subyard-e2e-bastion %s\n' "$(normalized_public_key_file "$IDENTITY.pub")" \
  > "$route_registry/test-yard/current/known_hosts"
BASTION_HOSTNAME=''; BASTION_PORT=''; BASTION_HOST_KEY_ALIAS=''; BASTION_KNOWN_HOSTS=''
SUBYARD_E2E_ROUTE_REGISTRY="$route_registry"
resolve_bastion_route
[ "$BASTION_HOSTNAME:$BASTION_PORT:$BASTION_HOST_KEY_ALIAS" = \
    10.24.0.8:22:subyard-e2e-bastion ] \
  || fail "root-published shared bastion route was not selected"
[ "$BASTION_KNOWN_HOSTS" = "$route_registry/test-yard/current/known_hosts" ] \
  || fail "product-owned bastion route lost its pinned host key"

status_fixture='{"schema_version":1,"status":"ok","capabilities":["attribution-v2"],"pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"lease_epoch":3,"state":"held","yard":"default","project":"Subyard-2","run":"run-a","purpose":"contract-tests","acquired_at":"2026-07-26T20:00:00Z","expires_at":"2026-07-26T20:20:00Z"},{"slot_id":"slot-002","resource_generation":1,"lease_epoch":2,"state":"available"},{"slot_id":"slot-003","resource_generation":1,"lease_epoch":0,"state":"available","acquired_at":"0001-01-01T00:00:00Z","expires_at":"0001-01-01T00:00:00Z"}]}}'
rendered_status="$(render_pool_status "$status_fixture")"
printf '%s\n' "$rendered_status" | grep -Fq 'SLOT     STATE' \
  && printf '%s\n' "$rendered_status" | grep -Fq 'Subyard-2' \
  || fail "human pool status omitted active-holder attribution"
! printf '%s\n' "$rendered_status" | grep -Fq '0001-01-01' \
  || fail "human pool status exposed zero timestamps"
long_project=Project-abcdefghijklmnopqrstuvwxyz0123456789
long_status="$(jq -c --arg project "$long_project" \
  '.pool.slots[0].project = $project' <<<"$status_fixture")"
long_rendered_status="$(render_pool_status "$long_status")"
grep -Fq "$long_project" <<<"$long_rendered_status" \
  || fail "human pool status truncated an attribution identifier"

# Model direct guest SSH and cleanup locally.
guest() {
  shift
  if [ "${1:-}" = sudo ] && [ "${2:-}" = -n ]; then shift 2; fi
  case "${1:-}" in
    /tmp/subyard-worktree.*/run.sh)
      sed \
        -e '/^chown -R dev:dev /d' \
        -e 's#^exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev #exec #' \
        "$1" | bash
      return
      ;;
  esac
  "$@"
}
mock_bundle="$TMP/mock.tar.gz"
tar -C "$fixture" -czf "$mock_bundle" tracked.txt
mock_hash="$(sha256sum "$mock_bundle" | awk '{print $1}')"
run_guest 1 "$mock_bundle" "$mock_hash" test -f tracked.txt \
  || fail "mock guest command failed"
guest_directory="${GUEST_DIRS[1]:-}"
case "$guest_directory" in /tmp/subyard-worktree.*) ;; *) fail "guest run directory was not retained for cleanup" ;; esac
[ -d "$guest_directory" ] || fail "mock guest run directory is missing"
cleanup_guest 1 || fail "guest run directory cleanup failed"
[ ! -e "$guest_directory" ] || fail "guest run directory survived cleanup"

set +e
bash -c '
  set -euo pipefail
  . "$1/dev/agent-e2e.sh"
  GUEST_DIRS[1]=/tmp/subyard-worktree.fixture
  cleanup_guest() { return 1; }
  cleanup_on_exit
' _ "$ROOT" >/dev/null 2>&1
cleanup_rc=$?
set -e
[ "$cleanup_rc" = 3 ] || fail "trap cleanup failure returned $cleanup_rc instead of 3"

if sed '/^[[:space:]]*#/d' "$ROOT/dev/agent-e2e.sh" \
  | grep -Eq 'test-vms[[:space:]]+(up|down)|yard[[:space:]].*(start|stop)'; then
  fail "agent E2E transport contains an allocation lifecycle call"
fi
if sed '/^[[:space:]]*#/d' "$ROOT/dev/e2e/p0-acceptance.sh" \
  | grep -Eq 'test-vms[[:space:]]+(up|down)|yard[[:space:]].*(start|stop)'; then
  fail "P0 acceptance contains an allocation lifecycle call"
fi
grep -Fq 'trap owner_cleanup EXIT' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not clean its candidate after failure"
grep -Fq 'prepare_owner_go_cache' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'p0_capacity_reset_build_cache' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'p0_capacity_remove_build_cache' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'env -u GOCACHE go clean -cache' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane leaves candidate Go caches on the disposable VM"
grep -Fq 'dev/build-engine.sh --force' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not build an explicit source candidate"
grep -Fq 'scripts/install-runtime-release.sh' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not install an immutable candidate runtime"
grep -Fq 'RENAME_BASE_REVISION=' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not install the real pre-rename runtime"
grep -Fq "grep -Fc 'Timeout:        10 * time.Minute,'" "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq "sed -i 's/Timeout:        10 \\* time.Minute,/Timeout:        30 * time.Minute,/'" \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not bound its synthetic legacy timeout override"
grep -Fq 'write_owner_registration e2e-yard e2e-vms' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not exercise the retired registration"
grep -Fq 'OWNER_DIAGNOSTIC_VM_MEMORY="${P0_E2E_DIAGNOSTIC_VM_MEMORY:-700MiB}"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'OWNER_DIAGNOSTIC_VM_MEMORY="${P0_BROKER_RECOVERY_VM_MEMORY:-700MiB}"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'E2E_VM_MEMORY=%s' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq ': "${E2E_VM_MEMORY:=4GiB}"' "$ROOT/scripts/e2e-lab/provision.sh" \
  || fail "P0 memory limit is not diagnostic-only or changed the production default"
grep -Fq 'OWNER_DIAGNOSTIC_VM_BOOT_TIMEOUT="${P0_E2E_DIAGNOSTIC_VM_BOOT_TIMEOUT:-600}"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'E2E_VM_BOOT_TIMEOUT=%s' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq ': "${E2E_VM_BOOT_TIMEOUT:=300}"' "$ROOT/scripts/e2e-lab/provision.sh" \
  || fail "P0 boot timeout is not diagnostic-only or changed the production default"
grep -Fq 'runtime activation retained the old e2e-yard registration' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not verify automatic retirement of the old yard"
grep -Fq 'features.images=false -c user.subyard.p0-image-cache="$MARKER"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not attach its test-owned image cache before fresh reconciliation"
owner_bootstrap_line="$(grep -n $'^\tensure_owner_incus$' "$ROOT/dev/e2e/p0-guest.sh" | head -n1 | cut -d: -f1)"
owner_incus_line="$(grep -n 'OWNER_BASELINE_IMAGES=.*incus image list' "$ROOT/dev/e2e/p0-guest.sh" | head -n1 | cut -d: -f1)"
[ -n "$owner_bootstrap_line" ] && [ -n "$owner_incus_line" ] \
  && [ "$owner_bootstrap_line" -lt "$owner_incus_line" ] \
  || fail "P0 owner lane uses Incus before its disposable-VM bootstrap"
grep -Fq './bin/yard -Y test-yard start --yes' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not make start automation explicit"
grep -Fq 'shell "$source" --yes --' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not confirm shell automation"
grep -Fq 'export "$source" --yes' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not confirm export automation"
grep -Fq 'exec %q/yard "$@"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq '"$release/subyard-install.sh" --yes' "$ROOT/dev/e2e/p0-guest.sh" \
  && ! grep -Fq 'YARD_ENGINE_PATH=%q' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer wrapper does not use its release-installed runtime"
grep -Fq 'PEER_YARD_ENTRY="$HOME/.local/bin/yard"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'VM1 user yard entry was not restored exactly' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && ! grep -Fq '/usr/local/bin/yard' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer wrapper does not preserve the login-PATH user entrypoint"
grep -Fq 'UserKnownHostsFile="$PEER_SSH_DIR/known_hosts"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'ConnectTimeout=8' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'id_ed25519"' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 cross-owner SSH lacks its synthetic identity, strict pin or bounded timeout"
grep -Fq 'remove_peer_authorization' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer cleanup does not revoke its synthetic SSH authorization"
credentials_line="$(grep -n '^peer_credentials()' "$ROOT/dev/e2e/p0-guest.sh" | cut -d: -f1)"
projects_line="$(grep -n '^peer_projects()' "$ROOT/dev/e2e/p0-guest.sh" | cut -d: -f1)"
remote_remove_line="$(grep -n 'remote remove peer --yes' "$ROOT/dev/e2e/p0-guest.sh" | cut -d: -f1)"
[ -n "$credentials_line" ] && [ -n "$projects_line" ] && [ -n "$remote_remove_line" ] \
  && [ "$remote_remove_line" -gt "$credentials_line" ] \
  && [ "$remote_remove_line" -lt "$projects_line" ] \
  || fail "P0 peer alias is removed before its credentials consumer finishes"
grep -Fq 'incus "$@" </dev/null; }' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'real_incus_quiet launch "$VM_IMAGE" p0-vm' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'CONTAINER_CACHE_ALIAS="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'VM_CACHE_ALIAS="${P0_REAL_INCUS_VM_CACHE_ALIAS:-' "$ROOT/dev/e2e/p0-real-incus.sh" \
  || fail "P0 real-Incus lane leaves YAML-reading control-plane stdin open"
grep -Fq 'wait_ready p0-container container' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'wait_ready p0-vm virtual-machine' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq -- '-c security.secureboot=false' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'P0_REAL_INCUS_RESTART_GRACE_ATTEMPTS:-30' \
    "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'stopped during first boot; replacing it once' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'relaunching real Incus VM after first-boot stop' "$ROOT/dev/e2e/p0-real-incus.sh" \
  || fail "P0 real-Incus lane does not bound first-boot VM recovery with deterministic boot policy"
grep -Fq 'cleanup delete of %s failed; retrying (%s/3)' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'refusing to delete unmarked instance' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'could not delete marked instance $name after 3 attempts' "$ROOT/dev/e2e/p0-real-incus.sh" \
  || fail "P0 real-Incus cleanup retry is not bounded to marked test instances"
real_incus_cleanup_functions="$(
  for function_name in real_incus_observe p0_monotonic_seconds cleanup_real_incus \
    cleanup_sleep active_instance_operation_ids delete_marked_instance; do
    sed -n "/^${function_name}() {/,/^}/p" "$ROOT/dev/e2e/p0-real-incus.sh"
  done
)"
run_absent_instance_cleanup() {
  P0_REAL_INCUS_CLEANUP_FUNCTIONS="$real_incus_cleanup_functions" \
    P0_REAL_INCUS_CLEANUP_CASE="$1" bash -c '
      set -euo pipefail
      MARKER=agent-e2e-p0
      PROJECT=subyard-p0-real-incus
      delete_attempted=0
      P0_REAL_INCUS_CLEANUP_WAIT_SECONDS=5
      P0_REAL_INCUS_CLEANUP_POLL_SECONDS=0
      P0_REAL_INCUS_DELETE_RETRY_SECONDS=0
      die() { printf "%s\n" "$*" >&2; exit 2; }
      eval "$P0_REAL_INCUS_CLEANUP_FUNCTIONS"
      real_incus() {
        case "$1 $2" in
          "project get"|"config get") printf "%s\\n" "$MARKER" ;;
          "delete p0-vm")
            delete_attempted=1
            return 1
            ;;
          "list p0-vm")
            case "$P0_REAL_INCUS_CLEANUP_CASE:$delete_attempted" in
              initial-absence:0|operation-query-error:0|unrelated-operation:0|\
                malformed-operation:0|malformed-operation-resource:0|\
                post-delete-absence:1) ;;
              post-delete-absence:0) printf "%s\\n" p0-vm ;;
              query-error:0) return 124 ;;
              *) return 3 ;;
            esac
            ;;
          "operation list")
            case "$P0_REAL_INCUS_CLEANUP_CASE" in
              operation-query-error) return 124 ;;
              unrelated-operation)
                printf "%s\\n" \
                  "[{\"id\":\"foreign\",\"status_code\":103,\"resources\":{\"instances\":[\"/1.0/instances/p0-other\"]}}]"
                ;;
              malformed-operation) printf "%s\\n" "{not-json" ;;
              malformed-operation-resource)
                printf "%s\\n" \
                  "[{\"id\":\"ambiguous\",\"status_code\":103,\"resources\":{\"instances\":[42]}}]"
                ;;
              *) printf "%s\\n" "[]" ;;
            esac
            ;;
          *) return 3 ;;
        esac
      }
      delete_marked_instance p0-vm
    '
}
for absent_cleanup_case in initial-absence post-delete-absence unrelated-operation; do
  set +e
  run_absent_instance_cleanup "$absent_cleanup_case"
  absent_cleanup_rc=$?
  set -e
  [ "$absent_cleanup_rc" = 0 ] \
    || fail "P0 real-Incus cleanup treats $absent_cleanup_case as a retry-fatal failure"
done
set +e
run_absent_instance_cleanup query-error
query_error_cleanup_rc=$?
set -e
[ "$query_error_cleanup_rc" -ne 0 ] \
  || fail 'P0 real-Incus cleanup mistakes an instance-observation error for absence'
for operation_failure_case in operation-query-error malformed-operation \
  malformed-operation-resource; do
  set +e
  run_absent_instance_cleanup "$operation_failure_case" >/dev/null 2>&1
  operation_failure_rc=$?
  set -e
  [ "$operation_failure_rc" -ne 0 ] \
    || fail "P0 real-Incus cleanup mistakes $operation_failure_case for stable absence"
done
set +e
P0_REAL_INCUS_CLEANUP_FUNCTIONS="$real_incus_cleanup_functions" bash -c '
  set -euo pipefail
  eval "$P0_REAL_INCUS_CLEANUP_FUNCTIONS"
  clock=100
  slept=-1
  p0_monotonic_seconds() { printf "%s\\n" "$clock"; }
  sleep() {
    slept="$1"
    clock=$((clock + slept))
  }
  cleanup_sleep 103 30
  [ "$slept" = 3 ] && [ "$clock" = 103 ]
' >/dev/null 2>&1
cleanup_sleep_rc=$?
set -e
[ "$cleanup_sleep_rc" = 0 ] \
  || fail 'P0 real-Incus cleanup sleep can overrun its total deadline'
real_incus_cleanup_functions="$real_incus_cleanup_functions
$(sed -n '/^launch_with_retry() {/,/^}/p' "$ROOT/dev/e2e/p0-real-incus.sh")"
set +e
P0_REAL_INCUS_CLEANUP_FUNCTIONS="$real_incus_cleanup_functions" \
  P0_REAL_INCUS_RACE_STATE="$TMP/real-incus-race" bash -c '
    set -euo pipefail
    MARKER=agent-e2e-p0
    PROJECT=subyard-p0-real-incus
    E2E_PROGRESS_INTERVAL=60
    P0_REAL_INCUS_CLEANUP_WAIT_SECONDS=5
    P0_REAL_INCUS_CLEANUP_POLL_SECONDS=0
    mkdir -p "$P0_REAL_INCUS_RACE_STATE"
    printf "0\n" > "$P0_REAL_INCUS_RACE_STATE/launches"
    printf "0\n" > "$P0_REAL_INCUS_RACE_STATE/operation-queries"
    die() { printf "%s\n" "$*" >&2; exit 2; }
    eval "$P0_REAL_INCUS_CLEANUP_FUNCTIONS"
    run_with_progress() {
      shift
      "$@"
    }
    real_incus() {
      local launches operation_queries
      case "$1 $2" in
        "project get") printf "%s\n" "$MARKER" ;;
        "list p0-vm") ;;
        "operation list")
          operation_queries="$(cat "$P0_REAL_INCUS_RACE_STATE/operation-queries")"
          operation_queries=$((operation_queries + 1))
          printf "%s\n" "$operation_queries" \
            > "$P0_REAL_INCUS_RACE_STATE/operation-queries"
          if [ "$operation_queries" = 1 ]; then
            printf "%s\n" \
              "[{\"id\":\"create-p0-vm\",\"status_code\":103,\"resources\":{\"instances\":[\"/1.0/instances/p0-vm\"]}}]"
          else
            printf "%s\n" "[]"
            touch "$P0_REAL_INCUS_RACE_STATE/settled"
          fi
          ;;
        *) return 3 ;;
      esac
    }
    fake_launch() {
      local launches
      launches="$(cat "$P0_REAL_INCUS_RACE_STATE/launches")"
      launches=$((launches + 1))
      printf "%s\n" "$launches" > "$P0_REAL_INCUS_RACE_STATE/launches"
      if [ "$launches" = 1 ]; then
        return 124
      fi
      if [ ! -e "$P0_REAL_INCUS_RACE_STATE/settled" ]; then
        touch "$P0_REAL_INCUS_RACE_STATE/early-retry"
        return 91
      fi
    }
    launch_with_retry p0-vm "race fixture" fake_launch
    [ "$(cat "$P0_REAL_INCUS_RACE_STATE/launches")" = 2 ]
    [ "$(cat "$P0_REAL_INCUS_RACE_STATE/operation-queries")" = 2 ]
    [ ! -e "$P0_REAL_INCUS_RACE_STATE/early-retry" ]
  '
real_incus_race_rc=$?
set -e
[ "$real_incus_race_rc" = 0 ] \
  || fail 'P0 real-Incus launch retry raced a still-active exact-name create operation'
grep -A3 -F 'real-incus)' "$ROOT/dev/e2e/p0-guest.sh" \
  | grep -Fq 'ensure_owner_incus real-incus' \
  || fail 'P0 real-Incus guest mode does not bootstrap Incus on a clean allocation'
grep -Fq 'guest "$vm" \' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 peer cleanup assertion bypasses direct-command argv quoting"
grep -Fq 'cleanup_peer_incus' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer lane does not clean its Incus fixture"
grep -Fq '. "$ROOT/tests/helpers/test-context.sh"' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'run_incus_installer --yes --zabbly' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && ! grep -Fq '"$ROOT/scripts/01-install-incus.sh" --yes --zabbly' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade bootstrap bypasses the typed test engine context"
grep -Fq 'POWER_RETRY_WRAPPER=' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq '"ExecStart=$POWER_RETRY_WRAPPER"' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'exec /usr/local/libexec/subyard/yard-boot-reconcile _power-reconcile' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && ! grep -Fq 'ExecStartPre=' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade TEMPFAIL probe does not exercise the main reconciler process"
power_systemd_lane_body="$(sed -n '/^power_systemd_lane() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-acceptance.sh")"
grep -Fq 'run_power_systemd_vm "$vm" dev/e2e/power-reconciler-systemd.sh' \
    <<<"$power_systemd_lane_body" \
  && grep -Fq 'run_power_systemd_vm "$vm" dev/e2e/power-reconciler-systemd-255.sh' \
    <<<"$power_systemd_lane_body" \
  || fail "P0 acceptance gate does not exercise real systemd before the release migration"
power_prepare_line="$(grep -nF \
  'run_power_systemd_vm "$vm" dev/e2e/power-reconciler-upgrade.sh prepare "$TOKEN"' \
  <<<"$power_systemd_lane_body" | cut -d: -f1 || true)"
power_first_reboot_line="$(grep -nF 'reboot_vm "$vm"' <<<"$power_systemd_lane_body" \
  | sed -n '1p' | cut -d: -f1 || true)"
power_resume_line="$(grep -nF \
  'run_power_systemd_vm "$vm" dev/e2e/power-reconciler-upgrade.sh resume "$TOKEN"' \
  <<<"$power_systemd_lane_body" | cut -d: -f1 || true)"
power_second_reboot_line="$(grep -nF 'reboot_vm "$vm"' <<<"$power_systemd_lane_body" \
  | sed -n '2p' | cut -d: -f1 || true)"
power_finish_line="$(grep -nF \
  'run_power_systemd_vm "$vm" dev/e2e/power-reconciler-upgrade.sh finish "$TOKEN"' \
  <<<"$power_systemd_lane_body" | cut -d: -f1 || true)"
[[ "$power_prepare_line" =~ ^[0-9]+$ ]] \
  && [[ "$power_first_reboot_line" =~ ^[0-9]+$ ]] \
  && [[ "$power_resume_line" =~ ^[0-9]+$ ]] \
  && [[ "$power_second_reboot_line" =~ ^[0-9]+$ ]] \
  && [[ "$power_finish_line" =~ ^[0-9]+$ ]] \
  && [ "$power_prepare_line" -lt "$power_first_reboot_line" ] \
  && [ "$power_first_reboot_line" -lt "$power_resume_line" ] \
  && [ "$power_resume_line" -lt "$power_second_reboot_line" ] \
  && [ "$power_second_reboot_line" -lt "$power_finish_line" ] \
  || fail "P0 exact v0.8.0 migration is not verified across two ordered host reboots"
upgrade_dispatcher="$(awk '
  /^case "\$MODE" in$/ { block = ""; capture = 1 }
  capture { block = block $0 ORS }
  END { printf "%s", block }
' "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
for dispatcher_mode in prepare resume finish; do
  dispatcher_log="$TMP/power-dispatcher-$dispatcher_mode.log"
  set +e
  UPGRADE_DISPATCHER="$upgrade_dispatcher" DISPATCHER_MODE="$dispatcher_mode" \
    DISPATCHER_LOG="$dispatcher_log" bash -c '
      set -euo pipefail
      MODE="$DISPATCHER_MODE"
      STATE_ROOT=/state
      PHASE_STATE=/state/phase
      CANDIDATE_VERSION=candidate
      ROOT=/candidate
      PRESERVE_FIXTURE=0
      CLEANUP_ARMED=0
      log() { printf "%s\n" "$*" >> "$DISPATCHER_LOG"; }
      incus() { log "incus $*"; }
      die() { log "die $*"; exit 2; }
      info() { :; }
      prepare_candidate() { log prepare_candidate; }
      record_reboot_baseline() { log record_reboot_baseline; }
      write_fixture_value() { log "write_fixture_value $*"; }
      finish_candidate_flow() { log finish_candidate_flow; }
      assert_state_root() { log assert_state_root; }
      assert_fixture_phase() { log "assert_fixture_phase $*"; }
      assert_post_reboot_candidate() { log assert_post_reboot_candidate; }
      operator_yard() { log "operator_yard $*"; }
      assert_release_state() { log "assert_release_state $*"; }
      trap '\''log "exit preserve=$PRESERVE_FIXTURE cleanup=$CLEANUP_ARMED"'\'' EXIT
      eval "$UPGRADE_DISPATCHER"
    ' >/dev/null 2>&1
  dispatcher_rc=$?
  set -e
  [ "$dispatcher_rc" = 0 ] \
    || fail "power reconciler $dispatcher_mode dispatcher rejected its valid phase"
  case "$dispatcher_mode" in
    prepare)
      dispatcher_expected=$'incus image info subyard-e2e-debian-13-cloud-container --project default\nprepare_candidate\nrecord_reboot_baseline\nwrite_fixture_value /state/phase candidate-ready\nexit preserve=1 cleanup=0'
      ;;
    resume)
      dispatcher_expected=$'assert_state_root\nassert_fixture_phase candidate-ready\nassert_post_reboot_candidate\noperator_yard init --yes\nassert_release_state candidate 5 /candidate/config/systemd/subyard-power-reconcile.service.in loaded\nrecord_reboot_baseline\nwrite_fixture_value /state/phase candidate-reconciled\nexit preserve=1 cleanup=1'
      ;;
    finish)
      dispatcher_expected=$'assert_state_root\nassert_fixture_phase candidate-reconciled\nassert_post_reboot_candidate\nfinish_candidate_flow\nexit preserve=0 cleanup=1'
      ;;
  esac
  [ "$(cat "$dispatcher_log")" = "$dispatcher_expected" ] \
    || fail "power reconciler $dispatcher_mode dispatcher lost its ordered incident flow"
done
grep -Fq 'OLD_VERSION=0.8.0' "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq \
    'OLD_INSTALLER_SHA256=5bd3c61e3dd39cb2d258be5cd75237383f00eff0512c77a3a5ca75d96e6b992b' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'subyard-power-reconcile-v0.8.0.service.in' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'repair-power-reconciler-systemd-compat' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'update --rollback --yes' "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail "power reconciler migration E2E lost exact release rollback coverage"
grep -Fq 'active|inactive|failed' "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'failed:failed' "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail 'power reconciler migration E2E cannot preserve a pre-existing failed host unit'
grep -Fq 'sudo -n install -D -o root -g root' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail 'power reconciler migration cleanup cannot restore a removed runtime directory'
grep -Fq 'assert_post_reboot_candidate()' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'BOOT_ID_STATE="$STATE_ROOT/boot-id"' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'DEFAULT_ROUTE_STATE="$STATE_ROOT/default-route"' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'ExecMainStartTimestampMonotonic' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'PRESERVE_FIXTURE=1' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'operator_yard init --yes' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail 'power reconciler reboot fixture does not prove current-boot success and idempotent convergence'
fixture_state_functions="$(
  sed -n '/^write_fixture_value() {/,/^}/p' "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
  sed -n '/^read_fixture_value() {/,/^}/p' "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
  sed -n '/^assert_fixture_phase() {/,/^}/p' "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
)"
(
  STATE_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/subyard-power-phase.XXXXXX")"
  PHASE_STATE="$STATE_ROOT/phase"
  trap 'find "$STATE_ROOT" -depth -delete' EXIT
  die() { return 2; }
  eval "$fixture_state_functions"
  write_fixture_value "$PHASE_STATE" candidate-ready
  assert_fixture_phase candidate-ready
  set +e
  assert_fixture_phase candidate-reconciled >/dev/null 2>&1
  wrong_phase_rc=$?
  set -e
  [ "$wrong_phase_rc" -ne 0 ] \
    || fail 'power reconciler phase guard accepted an out-of-order resume'
  write_fixture_value "$PHASE_STATE" candidate-reconciled
  assert_fixture_phase candidate-reconciled
) || fail 'power reconciler persisted phase state is not enforced behaviorally'

cleanup_function="$(sed -n '/^cleanup() {/,/^}/p' \
  "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
cleanup_log="$TMP/power-cleanup.log"
: > "$cleanup_log"
set +e
CLEANUP_FUNCTION="$cleanup_function" CLEANUP_LOG="$cleanup_log" bash -c '
  set -u
  eval "$CLEANUP_FUNCTION"
  PRESERVE_FIXTURE=1
  CLEANUP_ARMED=1
  p0_capacity_remove_build_cache() { printf "build\n" >> "$CLEANUP_LOG"; }
  p0_capacity_remove_root_if_empty() { printf "root\n" >> "$CLEANUP_LOG"; }
  cleanup
' >/dev/null 2>&1
preserve_cleanup_rc=$?
set -e
[ "$preserve_cleanup_rc" = 0 ] && [ ! -s "$cleanup_log" ] \
  || fail 'successful power phase did not preserve its reboot fixture'
: > "$cleanup_log"
set +e
CLEANUP_FUNCTION="$cleanup_function" CLEANUP_LOG="$cleanup_log" bash -c '
  set +e
  set -u
  eval "$CLEANUP_FUNCTION"
  PRESERVE_FIXTURE=1
  CLEANUP_ARMED=0
  p0_capacity_remove_build_cache() { printf "build\n" >> "$CLEANUP_LOG"; }
  p0_capacity_remove_root_if_empty() { printf "root\n" >> "$CLEANUP_LOG"; }
  false
  cleanup
' >/dev/null 2>&1
failed_cleanup_rc=$?
set -e
[ "$failed_cleanup_rc" -ne 0 ] \
  && grep -Fxq build "$cleanup_log" \
  && grep -Fxq root "$cleanup_log" \
  || fail 'failed power phase preserved state instead of entering cleanup'
find "$cleanup_log" -delete

assert_post_reboot_function="$(sed -n '/^assert_post_reboot_candidate() {/,/^}/p' \
  "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
for evidence_scenario in success same-boot route-change manager-failure yard-stopped; do
  set +e
  ASSERT_POST_REBOOT_FUNCTION="$assert_post_reboot_function" \
    EVIDENCE_SCENARIO="$evidence_scenario" bash -c '
      set -euo pipefail
      eval "$ASSERT_POST_REBOOT_FUNCTION"
      STATE_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/subyard-power-evidence.XXXXXX")"
      BOOT_ID_STATE="$STATE_ROOT/boot-id"
      DEFAULT_ROUTE_STATE="$STATE_ROOT/default-route"
      CANDIDATE_VERSION=candidate
      ROOT=/candidate
      PROJECT=subyard
      INSTANCE=yard
      printf "default via 192.0.2.1 dev eth0\n" > "$DEFAULT_ROUTE_STATE"
      trap '\''find "$STATE_ROOT" -depth -delete'\'' EXIT
      die() { exit 2; }
      read_fixture_value() { printf "old-boot\n"; }
      cat() {
        if [ "$EVIDENCE_SCENARIO" = same-boot ]; then
          printf "old-boot\n"
        else
          printf "new-boot\n"
        fi
      }
      ip() {
        if [ "$EVIDENCE_SCENARIO" = route-change ]; then
          printf "default via 192.0.2.254 dev eth0\n"
        else
          printf "default via 192.0.2.1 dev eth0\n"
        fi
      }
      load_release_targets() {
        OLD_RELEASE_TARGET=/runtime/old
        CANDIDATE_RELEASE_TARGET=/runtime/candidate
      }
      assert_release_state() { :; }
      assert_runtime_links() { :; }
      assert_candidate_transaction() { :; }
      sudo() {
        if [ "$EVIDENCE_SCENARIO" = manager-failure ]; then
          printf "%s\n" \
            LoadState=loaded NeedDaemonReload=no ActiveState=failed SubState=failed \
            Result=exit-code ExecMainStatus=75 ExecMainStartTimestampMonotonic=1000000
        else
          printf "%s\n" \
            LoadState=loaded NeedDaemonReload=no ActiveState=inactive SubState=dead \
            Result=success ExecMainStatus=0 ExecMainStartTimestampMonotonic=1000000
        fi
      }
      incus() {
        if [ "$EVIDENCE_SCENARIO" = yard-stopped ]; then
          printf "STOPPED\n"
        else
          printf "RUNNING\n"
        fi
      }
      assert_post_reboot_candidate
    ' >/dev/null 2>&1
  evidence_rc=$?
  set -e
  if [ "$evidence_scenario" = success ]; then
    [ "$evidence_rc" = 0 ] \
      || fail 'valid power reconciler reboot evidence was rejected'
  else
    [ "$evidence_rc" -ne 0 ] \
      || fail "power reconciler accepted invalid reboot evidence: $evidence_scenario"
  fi
done
grep -Fq 'subyard-power-reconcile-v0.7.2.service.in' \
  "$ROOT/tests/power-reconciler-systemd.sh" \
  || fail 'systemd 255 historical parser regression lost the v0.7.2 fixture'
incus_wrapper="$(sed -n '/^incus() {/,/^}/p' \
  "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
grep -Fq 'timeout --signal=TERM --kill-after="$INCUS_KILL_AFTER_SECONDS"' \
    <<<"$incus_wrapper" \
  && ! grep -Fq -- '--foreground' <<<"$incus_wrapper" \
  && grep -Fq '</dev/null' <<<"$incus_wrapper" \
  && grep -Fq 'INCUS_COMMAND_TIMEOUT="${SUBYARD_POWER_SYSTEMD_INCUS_TIMEOUT_SECONDS:-60}"' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail 'power reconciler upgrade leaves Incus commands unbounded'
grep -Fq 'project_presence()' "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'incus project list --format csv -c n' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'projects="$(incus project list --format csv -c n)" || return 2' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq '[ "$project_presence_rc" = 1 ] || cleanup_failed=1' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail 'power reconciler cleanup can mistake a failed project query for absence'
(
  eval "$(sed -n '/^project_presence() {/,/^}/p' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
  # shellcheck disable=SC2034
  PROJECT=subyard
  query_result=present
  incus() {
    [ "$*" = 'project list --format csv -c n' ] || return 3
    case "$query_result" in
      present) printf 'default\nsubyard\n' ;;
      absent) printf 'default\n' ;;
      error) return 124 ;;
    esac
  }
  project_presence || fail 'project presence rejected a successful exact listing'
  query_result=absent
  set +e
  project_presence
  absent_rc=$?
  query_result=error
  project_presence
  error_rc=$?
  set -e
  [ "$absent_rc" = 1 ] && [ "$error_rc" = 2 ] \
    || fail 'project presence does not distinguish absence from query failure'
)
(
  child_pid=''
  pid_file="$(mktemp "${TMPDIR:-/tmp}/subyard-timeout-kill.XXXXXX")"
  trap '
    [ -z "$child_pid" ] || kill -KILL "$child_pid" >/dev/null 2>&1 || true
    find "$pid_file" -delete >/dev/null 2>&1 || true
  ' EXIT
  started=$SECONDS
  {
    set +e
    timeout --signal=TERM --kill-after=1 1 bash -c '
      trap "" TERM
      sleep 30 &
      printf "%s\n" "$!" > "$1"
      wait
    ' _ "$pid_file" >/dev/null 2>&1
    timeout_rc=$?
    set -e
  } 2>/dev/null
  child_pid="$(cat "$pid_file")"
  [ "$timeout_rc" = 137 ] \
    && [ "$((SECONDS - started))" -le 4 ] \
    && ! kill -0 "$child_pid" >/dev/null 2>&1 \
    || fail 'TERM-ignoring timeout fixture or descendant escaped the KILL deadline'
  child_pid=''
)
grep -Fq '. "$ROOT/dev/e2e/lib-p0-capacity.sh"' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'p0_capacity_reset_build_cache' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  && grep -Fq 'p0_capacity_remove_build_cache' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail "power reconciler migration E2E can pollute the shared Go build cache"
assert_unit_body="$(sed -n '/^assert_unit_matches() {/,/^}/p' \
  "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
! grep -Fq 'daemon-reload' <<<"$assert_unit_body" \
  && grep -Fq -- '--property=LoadState --property=NeedDaemonReload' <<<"$assert_unit_body" \
  && grep -Fq 'RestartForceExitStatus=75' <<<"$assert_unit_body" \
  && grep -Fq 'StartLimitIntervalUSec=15min' <<<"$assert_unit_body" \
  && grep -Fq 'power-reconciler-systemd-compat-v1' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh" \
  || fail 'power reconciler upgrade assertion mutates or incompletely observes manager state'
grep -Fq 'unit_state_snapshot()' "$ROOT/dev/e2e/power-reconciler-systemd.sh" \
  && ! grep -Fq 'unit_property()' "$ROOT/dev/e2e/power-reconciler-systemd.sh" \
  && grep -Fq 'attempts="$((attempts - 1))"' \
    "$ROOT/dev/e2e/power-reconciler-systemd.sh" \
  && grep -Fq 'has_unit_state inactive dead success 0' \
    "$ROOT/dev/e2e/power-reconciler-systemd.sh" \
  || fail 'real-PID1 power test depends on separate property reads or restart-counter retry control'
grep -Fq 'boot_power_reconciler_succeeded()' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'POWER_RECONCILE_WINDOW_SECONDS=900' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'POWER_RECONCILE_TERMINAL_FAILURE=1' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq "grep -Fxq 'SubState=failed'" \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'if [ "$POWER_RECONCILE_TERMINAL_FAILURE" = 1 ]; then' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -A5 -F 'power_reconcile_ssh()' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
    | grep -Fq 'timeout --foreground "$POWER_RECONCILE_PROBE_TIMEOUT"' \
  && grep -A7 -F 'power_reconcile_ssh()' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
    | grep -Fq -- '-o ConnectionAttempts=1' \
  && grep -Fq 'snapshot="$(power_reconcile_ssh systemctl show' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'uptime_seconds="$(power_reconcile_ssh cut -d. -f1 /proc/uptime)"' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'policy_deadline=$((started_seconds + POWER_RECONCILE_WINDOW_SECONDS))' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'if [ "$POWER_RECONCILE_UPTIME_SECONDS" -ge "$policy_deadline" ]; then' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq -- '--property=ActiveState --property=SubState --property=Result' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq -- '--property=ExecMainStatus' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq -- '--property=ExecMainStartTimestampMonotonic' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq '[[ "$started" =~ ^[1-9][0-9]*$ ]]' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'ActiveState=inactive' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'SubState=dead' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'Result=success' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'ExecMainStatus=0' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && ! grep -Fq '[ "$unit_result" != success ] || break' \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'P0 reboot can treat an active Type=exec reconciler as completed'
boot_power_reconciler_function="$(sed -n \
  '/^boot_power_reconciler_succeeded() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-acceptance.sh")"
terminal_failure_snapshot="$(
  BOOT_POWER_RECONCILER_FUNCTION="$boot_power_reconciler_function" bash -c '
    set -euo pipefail
    eval "$BOOT_POWER_RECONCILER_FUNCTION"
    power_reconcile_ssh() {
      if [ "$1" = systemctl ]; then
        printf "%s\n" \
          "ActiveState=failed" "SubState=failed" "Result=exit-code" \
          "ExecMainStatus=75" "ExecMainStartTimestampMonotonic=1000000"
      else
        printf "2\n"
      fi
    }
    if boot_power_reconciler_succeeded; then
      exit 1
    fi
    printf "%s\n" "$POWER_RECONCILE_TERMINAL_FAILURE"
  '
)"
[ "$terminal_failure_snapshot" = 1 ] \
  || fail 'P0 reboot does not terminate after systemd exhausts exit-75 retries'
retryable_failure_snapshot="$(
  BOOT_POWER_RECONCILER_FUNCTION="$boot_power_reconciler_function" bash -c '
    set -euo pipefail
    eval "$BOOT_POWER_RECONCILER_FUNCTION"
    power_reconcile_ssh() {
      if [ "$1" = systemctl ]; then
        printf "%s\n" \
          "ActiveState=activating" "SubState=auto-restart" "Result=exit-code" \
          "ExecMainStatus=75" "ExecMainStartTimestampMonotonic=1000000"
      else
        printf "2\n"
      fi
    }
    if boot_power_reconciler_succeeded; then
      exit 1
    fi
    printf "%s\n" "$POWER_RECONCILE_TERMINAL_FAILURE"
  '
)"
[ "$retryable_failure_snapshot" = 0 ] \
  || fail 'P0 reboot treats an auto-restarting exit-75 attempt as terminal'
grep -Fxq 'Wants=network-online.target incus.service incus.socket' \
    "$ROOT/config/systemd/subyard-power-reconcile.service.in" \
  || fail 'boot power reconciler can race cold Incus socket activation'
! grep -Eq '^[[:space:]]*incus (exec|delete)' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'timeout --signal=TERM --kill-after="$INCUS_KILL_AFTER_SECONDS" "$deadline"' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && ! grep -Fq -- '--foreground' "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq '"$@" </dev/null' "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'for attempt in 1 2 3' "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'assert_owned_project && assert_owned_instance' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  || fail 'systemd-255 Incus commands or marked cleanup are not fully bounded'
grep -Fq 'PROJECT_CREATED=0' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'if [ "$PROJECT_CREATED" = 1 ]; then' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'project lookup failed during cleanup' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  || fail 'systemd-255 cleanup can mistake a failed project lookup for absence'
grep -Fq 'scripts/install-power-reconciler.sh' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'systemctl enable "$INSTALL_UNIT"' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq -- '--property=LoadState --property=NeedDaemonReload' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'production installer left stale systemd 255 manager state' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  || fail 'systemd-255 lane does not exercise production reload and manager freshness'
grep -Fq 'INSTALL_UNIT_PATH="/etc/systemd/system/$INSTALL_UNIT"' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'subyard-power-reconcile-v0.8.0.service.in' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'restart_incus "$INSTANCE" --project "$PROJECT"' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'boot ID did not change across the systemd 255 fixture restart' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'ExecMainStartTimestampMonotonic' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  && grep -Fq 'systemd 255 candidate did not reach current-boot terminal success' \
    "$ROOT/dev/e2e/power-reconciler-systemd-255.sh" \
  || fail 'systemd-255 fixture does not prove the persistent candidate starts after PID1 restart'
grep -Fq 'if ! systemctl is-enabled --quiet "$UNIT_NAME"; then' \
    "$ROOT/scripts/install-power-reconciler.sh" \
  || fail 'power reconciler installer can hide a missing daemon-reload behind redundant enablement'
grep -Fq 'operator_yard -Y "$YARD_NAME" stop --yes' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'wait_for_desired_yards RUNNING STOPPED' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'wait_for_desired_yards STOPPED RUNNING' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'named stopped and default running desired power' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'power-reconciler-systemd-compat-v1' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail 'source-upgrade does not cover complementary reboot power states and exact operation identity'
! grep -Fq '"$SOURCE_ROOT/config/qa-pool/"*' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade fixture expands operator-private paths as the outer user"
grep -Fq 'AGENTS=codex\nCODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_RULES=' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade spends its legacy init deadline on unrelated agent downloads"
grep -Fq 'relax_fixture_init_deadline' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq "\$'\\t\\t\\tTimeout:        10 * time.Minute,'" \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq "\$'\\t\\t\\tTimeout:        30 * time.Minute,'" \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq $'\t\t\tTimeout:        10 * time.Minute,' "$ROOT/internal/cli/cli.go" \
  || fail "P0 source-upgrade does not bound its synthetic init without changing production"
source_deadline_call_line="$(grep -nF '  relax_fixture_init_deadline' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" | cut -d: -f1)"
source_build_line="$(grep -nF '  operator_env env YARD_BUILD_VERSION=' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" | cut -d: -f1)"
[[ "$source_deadline_call_line" =~ ^[0-9]+$ ]] \
  && [[ "$source_build_line" =~ ^[0-9]+$ ]] \
  && [ "$source_deadline_call_line" -lt "$source_build_line" ] \
  || fail "P0 source-upgrade relaxes its synthetic deadline after building the CLI"
source_deadline_function="$(awk '
  /^relax_fixture_init_deadline\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/p0-source-upgrade.sh")"
run_source_deadline_fixture() {
  local source_root="$1"
  SOURCE_ROOT="$source_root" bash -c '
    set -euo pipefail
    operator_env() { "$@"; }
    die() { printf "%s\n" "$*" >&2; return 2; }
    eval "$1"
    relax_fixture_init_deadline
  ' _ "$source_deadline_function"
}
source_deadline_root="$TMP/source-deadline"
mkdir -p "$source_deadline_root/internal/cli"
printf 'package cli\n%s\n' $'\t\t\tTimeout:        10 * time.Minute,' \
  > "$source_deadline_root/internal/cli/cli.go"
run_source_deadline_fixture "$source_deadline_root" \
  || fail "P0 source-upgrade rejected its exact synthetic deadline fixture"
grep -Fqx $'\t\t\tTimeout:        30 * time.Minute,' \
    "$source_deadline_root/internal/cli/cli.go" \
  && ! grep -Fq $'\t\t\tTimeout:        10 * time.Minute,' \
    "$source_deadline_root/internal/cli/cli.go" \
  || fail "P0 source-upgrade did not patch only the synthetic adapter deadline"
for invalid_deadline_count in missing duplicate mixed; do
  case "$invalid_deadline_count" in
    missing) printf 'package cli\n' > "$source_deadline_root/internal/cli/cli.go" ;;
    duplicate)
      printf '%s\n%s\n' \
        $'\t\t\tTimeout:        10 * time.Minute,' \
        $'\t\t\tTimeout:        10 * time.Minute,' \
        > "$source_deadline_root/internal/cli/cli.go"
      ;;
    mixed)
      printf '%s\n%s\n' \
        $'\t\t\tTimeout:        10 * time.Minute,' \
        $'\t\t\tTimeout:        30 * time.Minute,' \
        > "$source_deadline_root/internal/cli/cli.go"
      ;;
  esac
  set +e
  source_deadline_failure="$(run_source_deadline_fixture "$source_deadline_root" 2>&1)"
  source_deadline_rc=$?
  set -e
  [ "$source_deadline_rc" = 2 ] \
    && grep -Fq 'source-upgrade adapter timeout fixture no longer matches its source' \
      <<<"$source_deadline_failure" \
    || fail "P0 source-upgrade accepted a $invalid_deadline_count deadline fixture"
done
grep -Fq 's/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade lane does not verify the retired template migration"
grep -Fq 'migration_transaction_directory "$VERSION_B"' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'to_release="$(jq -er ".toRelease' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && ! grep -Fq '[ "${#entries[@]}" -eq 1 ]' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade lane does not select its journal by release identity"
grep -Fq 'OLD_VERSION=0.3.1' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'MISSED_VERSION=0.4.0' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && [ "$(grep -Fc 'AGENTS=' "$ROOT/dev/e2e/release-migration-catch-up.sh")" -ge 2 ] \
  && ! grep -Fq 'CODING_TOOL_INTEGRATIONS=none' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && ! grep -Fq '"INCUS_PROJECT=$LEGACY_PROJECT"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && ! grep -Fq '"INSTANCE_NAME=$LEGACY_INSTANCE"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'host_incus config device get "$CONSUMER_INSTANCE"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'running standard broker acquire from the pre-existing consumer' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'consumer restarted during route reconciliation' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'verify_legacy_power_rollback_cycle' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'operator_yard update --rollback --yes' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'ordinary catch-up rollback did not restore legacy desired power' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'guest_project="/srv/workspaces/Subyard-release-catchup-${RUN_ID}-vm${VM:-unknown}"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'guest_source="$guest_project/src"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'test ! -e "$1" && test ! -L "$1"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'find "$guest_source" -xdev -exec chown -h dev:dev' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'find "$guest_project" -xdev -depth -delete' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'power-reconciler-systemd-compat-v1' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up lanes do not cover both published histories and live consumer routing"
grep -Fq '| `power-systemd` |' "$ROOT/docs/test-vms.md" \
  && grep -Fq '`SUBYARD_P0_WAIT_SECONDS`' "$ROOT/docs/test-vms.md" \
  && grep -Fq '`SUBYARD_P0_FULL_MATRIX_TIMEOUT_SECONDS`' "$ROOT/docs/test-vms.md" \
  && grep -Fq '"fixture:power-systemd"' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'public P0 documentation or checkpoint inventory omits the power-systemd lane'
grep -Fq 'cleanup_owned_host_incus' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq '[ "$source" = "$PLATFORM_STORAGE" ]' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'host_incus storage delete default --project default' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up cleanup can leave its fixture-owned default Incus pool behind"
grep -Fq 'seal_state_root' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'touch "$STATE_ROOT/public-worktree.tar.gz"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'sudo -n chown root:root "$STATE_ROOT" "$STATE_ROOT/.marker"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'sudo -n chmod 0644 "$STATE_ROOT/.marker"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'sudo -n find "$STATE_ROOT" -depth -delete' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up leaves the operator runtime beneath an unsafe fixture-owned ancestor"
grep -Fq 'BROKEN_VERSION=0.4.1' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'RELEASE_040_TARGET=releases/0.4.0-68b9925f6880' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'RELEASE_041_TARGET=releases/0.4.1-fc5b03078508' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'validate_hotfix_transaction rolling-back rolling-back' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'sync -f "$(dirname "$journal")"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'published 0.4.0 -> broken 0.4.1 -> recovered 0.4.3 hotfix lane passed' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up hotfix lane does not cover exact published recovery and candidate retry"
grep -Fq 'clean published 0.4.0 -> 0.4.3 hotfix lane passed' \
  "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'legacy published 0.4.0 owner -> 0.4.3 hotfix lane passed' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'require_operator_password_sudo' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'operator_env sudo -k' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'spawn -noecho $env(SUBYARD_YARD_BIN) update' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'operator unexpectedly retained passwordless sudo' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && [ "$(grep -Fc '  upgrade_candidate present' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh")" -eq 4 ] \
  || fail "release hotfix lanes do not exercise ordinary updates with cold password-required sudo"
grep -Fq 'FAILED_HOTFIX_VERSION=0.4.2' \
  "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'RELEASE_042_TARGET=releases/0.4.2-17608894ab09' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'validate_failed_hotfix_transaction' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'repair_failed_hotfix_operation test-vm-broker-runtime' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'published 0.4.0 -> broken 0.4.2 -> recovered 0.4.3 hotfix lane passed' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up does not reproduce and recover the exact broken 0.4.2 transaction"
grep -Fq 'require_operator_password_sudo' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'assert_operator_password_sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'restore_operator_passwordless_sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'operator unexpectedly retained passwordless sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && [ "$(grep -Fc '  require_operator_password_sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh")" -eq 2 ] \
  || fail "P0 source-upgrade reboots do not preserve a password-required operator boundary"
grep -Fq 'dev/agent-e2e.sh --wait 20m --vm both' \
  "$ROOT/dev/e2e/release-migration-consumer.sh" \
  && grep -Fq 'dev/agent-e2e.sh --verify-boundary' \
    "$ROOT/dev/e2e/release-migration-consumer.sh" \
  || fail "release catch-up consumer bypasses the standard broker facade"
grep -Fq 'select(.slot_id == $slot)' "$ROOT/dev/agent-e2e.sh" \
  && grep -Fq 'current lease slot is absent from pool status' "$ROOT/dev/agent-e2e.sh" \
  || fail "agent E2E boundary verification is coupled to unrelated concurrent slots"
! grep -Fq 'test-vms-inner' "$ROOT/dev/agent-e2e.sh" \
  || fail "agent E2E transport still invokes the privileged lifecycle worker"

printf 'ok: agent E2E lease transport is pinned, fenced and cleanup-owned\n'
