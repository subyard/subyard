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

staging_bundle="$TMP/staging-bundle.tar.gz"
unreachable_sentinel="$TMP/unreachable-finalization"
late_staging_path="$TMP/late-staging-path"
late_cleanup_path="$TMP/late-cleanup-path"
command_transfer_state="$TMP/command-transfer-state"
cleanup_retry_state="$TMP/cleanup-retry-state"
: > "$staging_bundle"
set +e
(
  GUEST_DIRS=()
  guest() { return 255; }
  helper_rc=0
  run_guest 2 "$staging_bundle" ignored true || helper_rc=$?
  printf '%s\n' cleanup release > "$unreachable_sentinel"
  [ "$helper_rc" = 2 ]
) >"$TMP/unreachable-staging.log" 2>&1
unreachable_rc=$?
(
  GUEST_DIRS=()
  staged=/tmp/subyard-worktree.staging-failure
  guest() {
    local vm="$1"
    shift
    case "${1:-}" in
      mktemp) printf '%s\n' "$staged" ;;
      dd) return 23 ;;
      sudo)
        [ "$*" = "sudo -n find $staged -depth -delete" ] || return 97
        printf '%s\n' "$staged" > "$late_cleanup_path"
        ;;
      *) return 98 ;;
    esac
  }
  helper_rc=0
  run_guest 2 "$staging_bundle" ignored true || helper_rc=$?
  printf '%s\n' "${GUEST_DIRS[2]:-}" > "$late_staging_path"
  cleanup_guest 2
  [ "$helper_rc" = 2 ] && [ -z "${GUEST_DIRS[2]:-}" ]
) >"$TMP/late-staging.log" 2>&1
late_staging_rc=$?
(
  GUEST_DIRS=()
  staged=/tmp/subyard-worktree.command-transfer
  guest() {
    local vm="$1"
    shift
    case "${1:-}" in
      mktemp) printf '%s\n' "$staged" ;;
      dd)
        case "$*" in
          *"of=$staged/worktree.tar.gz"*) return 0 ;;
          *"of=$staged/run.sh"*) return 23 ;;
          *) return 97 ;;
        esac
        ;;
      sha256sum) printf 'expected  %s/worktree.tar.gz\n' "$staged" ;;
      mkdir|tar) return 0 ;;
      test) return 1 ;;
      sudo)
        [ "$*" = "sudo -n find $staged -depth -delete" ] || return 98
        return 0
        ;;
      *) return 99 ;;
    esac
  }
  helper_rc=0
  run_guest 2 "$staging_bundle" expected true || helper_rc=$?
  staged_before_cleanup="${GUEST_DIRS[2]:-}"
  cleanup_guest 2
  printf '%s|%s|%s\n' \
    "$helper_rc" "$staged_before_cleanup" "${GUEST_DIRS[2]:-}" > "$command_transfer_state"
) >"$TMP/command-transfer.log" 2>&1
command_transfer_rc=$?
(
  GUEST_DIRS=([2]=/tmp/subyard-worktree.cleanup-retry)
  cleanup_attempts=0
  guest() {
    local vm="$1"
    shift
    [ "$vm" = 2 ] \
      && [ "$*" = "sudo -n find /tmp/subyard-worktree.cleanup-retry -depth -delete" ] \
      || return 97
    cleanup_attempts=$((cleanup_attempts + 1))
    [ "$cleanup_attempts" -gt 1 ] || return 255
  }
  first_rc=0
  cleanup_guest 2 || first_rc=$?
  printf '%s|%s\n' "$first_rc" "${GUEST_DIRS[2]:-}" > "$cleanup_retry_state"
  second_rc=0
  cleanup_guest 2 || second_rc=$?
  printf '%s|%s\n' "$second_rc" "${GUEST_DIRS[2]:-}" >> "$cleanup_retry_state"
  printf 'attempts=%s\n' "$cleanup_attempts" >> "$cleanup_retry_state"
) >"$TMP/cleanup-retry.log" 2>&1
cleanup_retry_rc=$?
set -e
[ "$unreachable_rc" = 0 ] \
  && [ "$(cat "$unreachable_sentinel" 2>/dev/null || true)" = $'cleanup\nrelease' ] \
  && [ "$late_staging_rc" = 0 ] \
  && [ "$(cat "$late_staging_path" 2>/dev/null || true)" = /tmp/subyard-worktree.staging-failure ] \
  && [ "$(cat "$late_cleanup_path" 2>/dev/null || true)" = /tmp/subyard-worktree.staging-failure ] \
  && [ "$command_transfer_rc" = 0 ] \
  && [ "$(cat "$command_transfer_state" 2>/dev/null || true)" = \
    '2|/tmp/subyard-worktree.command-transfer|' ] \
  && [ "$cleanup_retry_rc" = 0 ] \
  && [ "$(cat "$cleanup_retry_state" 2>/dev/null || true)" = \
    $'255|/tmp/subyard-worktree.cleanup-retry\n0|\nattempts=2' ] \
  || fail "guest staging failure aborts caller-owned cleanup or loses its staged path"

p0_cleanup_function_source="$(
  for function_name in run_source_vm clean_source_host armed_fixture_vm \
    cleanup_full_source_arm cleanup_armed_full_fixtures cleanup; do
    sed -n "/^${function_name}() {/,/^}/p" "$ROOT/dev/e2e/p0-acceptance.sh"
  done
)"
p0_cleanup_events="$TMP/p0-cleanup-events"
p0_source_arm="$TMP/p0-source-arm"
printf '2\n' > "$p0_source_arm"
set +e
# These variables are consumed by the extracted P0 cleanup functions.
# shellcheck disable=SC2034
(
  set +e
  eval "$p0_cleanup_function_source"
  GUEST_DIRS=()
  P0_BUNDLE="$staging_bundle"
  P0_BUNDLE_HASH=ignored
  TOKEN=cleanup-fixture
  FULL_SOURCE_ARM_FILE="$p0_source_arm"
  FULL_POWER_ARM_FILE=
  SOURCE_LANE_VM=2
  SOURCE_ARCHIVE=
  SOURCE_ARCHIVE_REMOTE=
  P0_EVIDENCE="$TMP/p0-evidence.json"
  P0_FAILURE_LOG="$TMP/p0-failure.log"
  P0_CURRENT_PHASE=fixture
  P0_PHASE_STARTED=5
  PROBE_PID=
  PROBE_NAME=
  PROBE_MARKER=
  PEERS_READY=0
  SOURCE_HOST_STARTED=0
  POWER_SYSTEMD_STARTED=0
  POWER_SYSTEMD_LANE_VM=2
  PROBE_LOG=
  CAPACITY_LOG_DIR=
  LEASE_KEEPER_PID=
  LOCAL_TEMP=
  export P0_BUNDLE P0_BUNDLE_HASH TOKEN FULL_SOURCE_ARM_FILE FULL_POWER_ARM_FILE \
    SOURCE_LANE_VM SOURCE_ARCHIVE SOURCE_ARCHIVE_REMOTE P0_EVIDENCE P0_FAILURE_LOG \
    P0_CURRENT_PHASE P0_PHASE_STARTED PROBE_PID PROBE_NAME PROBE_MARKER PEERS_READY \
    SOURCE_HOST_STARTED POWER_SYSTEMD_STARTED POWER_SYSTEMD_LANE_VM PROBE_LOG \
    CAPACITY_LOG_DIR
  guest() { return 255; }
  collect_failure_diagnostics() { printf 'diagnostics:%s\n' "$*" >> "$p0_cleanup_events"; }
  stop_runner_children() { printf 'stop-runners\n' >> "$p0_cleanup_events"; }
  stop_capacity_monitors() { printf 'stop-capacity\n' >> "$p0_cleanup_events"; }
  p0_monotonic_seconds() { printf '10\n'; }
  write_evidence() { printf 'evidence:%s\n' "$*" >> "$p0_cleanup_events"; }
  release_lease() { printf 'release\n' >> "$p0_cleanup_events"; }
  false
  cleanup
) >"$TMP/p0-cleanup.log" 2>&1
p0_cleanup_rc=$?
set -e
[ "$p0_cleanup_rc" = 3 ] \
  && [ "$(cat "$p0_cleanup_events" 2>/dev/null || true)" = \
    $'diagnostics:failure-entry truncate\nstop-runners\nstop-capacity\ndiagnostics:post-stop append\nevidence:fixture failed 1 5\nrelease' ] \
  || fail "P0 armed-fixture staging failure bypasses post-stop evidence or lease release"

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
set_requested_slot 1 --slot
[ "$LEASE_REQUESTED_SLOT" = slot-001 ] \
  || fail "exact --slot did not resolve slot-001"
if (set_requested_slot 0 --slot) >/dev/null 2>&1; then
  fail "exact --slot accepted slot zero"
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
LEASE_RUN="$run_a"
LEASE_PURPOSE=contract-tests
LEASE_GENERATION=7
LEASE_REQUESTED_SLOT='slot-002'
lease_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$lease_request" = \
    "acquire-v2 client SHA256:key default Subyard-2 $run_a contract-tests ssh-ed25519 keyblob slot-002" ] \
  || fail "runner did not carry canonical attribution through acquire-v2"
exact_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$exact_request" = \
  "acquire-v2 client SHA256:key default Subyard-2 $run_a contract-tests ssh-ed25519 keyblob slot-002" ] \
  || fail "runner did not retain the existing exact-slot acquire protocol"
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

cli_fixture_bin="$TMP/acceptance-cli-bin"
cli_mktemp_log="$TMP/acceptance-cli-mktemp.log"
cli_output="$TMP/acceptance-cli-output.log"
mkdir -p "$cli_fixture_bin" "$TMP/acceptance-cli-tmp"
cat > "$cli_fixture_bin/mktemp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CLI_MKTEMP_LOG"
exec /usr/bin/mktemp "$@"
EOF
chmod +x "$cli_fixture_bin/mktemp"

run_p0_cli_fixture() {
  : > "$cli_mktemp_log"
  : > "$cli_output"
  set +e
  PATH="$cli_fixture_bin:$PATH" \
    CLI_MKTEMP_LOG="$cli_mktemp_log" \
    TMPDIR="$TMP/acceptance-cli-tmp" \
    SUBYARD_E2E_STATE_DIR="$TMP/p0-cli-state" \
    "$ROOT/dev/e2e/p0-acceptance.sh" "$@" >"$cli_output" 2>&1
  P0_CLI_RC=$?
  set -e
}

run_p0_cli_fixture --help
[ "$P0_CLI_RC" = 0 ] && [ ! -s "$cli_mktemp_log" ] \
  || fail 'P0 help requires a slot or initializes lease state'
run_p0_cli_fixture --list-lanes
[ "$P0_CLI_RC" = 0 ] && [ ! -s "$cli_mktemp_log" ] \
  || fail 'P0 lane listing requires a slot or initializes lease state'
p0_lane_inventory="$(cat "$cli_output")"

run_p0_cli_fixture
[ "$P0_CLI_RC" = 2 ] \
  && grep -Fq -- '--slot N is required' "$cli_output" \
  && [ ! -s "$cli_mktemp_log" ] \
  || fail 'P0 default mode reached temporary or lease state without --slot N'
while IFS=$'\t' read -r p0_lane _; do
  [ -n "$p0_lane" ] || continue
  [ "$p0_lane" != full ] || continue
  run_p0_cli_fixture --lane "$p0_lane"
  [ "$P0_CLI_RC" = 2 ] \
    && grep -Fq -- '--slot N is required' "$cli_output" \
    && [ ! -s "$cli_mktemp_log" ] \
    || fail "P0 lane $p0_lane reached temporary or lease state without --slot N"
done <<<"$p0_lane_inventory"

run_p0_cli_fixture --slot
[ "$P0_CLI_RC" = 2 ] \
  && grep -Fq -- '--slot needs a number from 1 to 999' "$cli_output" \
  && [ ! -s "$cli_mktemp_log" ] \
  || fail 'P0 accepted a missing --slot value'
run_p0_cli_fixture --slot 0
[ "$P0_CLI_RC" = 2 ] \
  && grep -Fq -- '--slot needs a number from 1 to 999' "$cli_output" \
  && [ ! -s "$cli_mktemp_log" ] \
  || fail 'P0 accepted an invalid --slot value'
run_p0_cli_fixture --slot 1 --slot 2
[ "$P0_CLI_RC" = 2 ] \
  && grep -Fq -- '--slot may be specified only once' "$cli_output" \
  && [ ! -s "$cli_mktemp_log" ] \
  || fail 'P0 accepted duplicate --slot values'

: > "$cli_mktemp_log"
set +e
PATH="$cli_fixture_bin:$PATH" \
  CLI_MKTEMP_LOG="$cli_mktemp_log" \
  TMPDIR="$TMP/acceptance-cli-tmp" \
  SUBYARD_E2E_STATE_DIR="$TMP/p0-cli-state" \
  SUBYARD_P0_SLOT=2 \
  "$ROOT/dev/e2e/p0-acceptance.sh" --lane cleanup >"$cli_output" 2>&1
p0_env_slot_rc=$?
set -e
[ "$p0_env_slot_rc" = 2 ] \
  && grep -Fq -- '--slot N is required' "$cli_output" \
  && [ ! -s "$cli_mktemp_log" ] \
  || fail 'P0 retained the SUBYARD_P0_SLOT compatibility path'

p0_source_without_comments="$(sed '/^[[:space:]]*#/d' "$ROOT/dev/e2e/p0-acceptance.sh")"
[ "$(grep -Ec '^[[:space:]]*acquire_lease([[:space:]]|$)' \
  <<<"$p0_source_without_comments")" = 1 ] \
  && ! grep -Fq 'SUBYARD_P0_SLOT' <<<"$p0_source_without_comments" \
  && ! grep -Eq 'LEASE_(EXCLUDED|EXCLUSION)|exclude_.*slot|slot_.*exclude' \
    <<<"$p0_source_without_comments" \
  || fail 'P0 does not have exactly one explicit outer acquire or retains slot exclusion'

run_entity_cli_fixture() {
  : > "$cli_mktemp_log"
  : > "$cli_output"
  set +e
  PATH="$cli_fixture_bin:$PATH" \
    CLI_MKTEMP_LOG="$cli_mktemp_log" \
    TMPDIR="$TMP/acceptance-cli-tmp" \
    SUBYARD_E2E_STATE_DIR="$TMP/entity-cli-state" \
    "$ROOT/dev/e2e/entity-naming-acceptance.sh" "$@" >"$cli_output" 2>&1
  ENTITY_CLI_RC=$?
  set -e
}

run_entity_cli_fixture
[ "$ENTITY_CLI_RC" = 2 ] \
  && grep -Fq -- '--slot N is required' "$cli_output" \
  && [ ! -s "$cli_mktemp_log" ] \
  && [ ! -e "$TMP/entity-cli-state/id_ed25519" ] \
  || fail 'entity naming acceptance initialized temp/key state before requiring --slot N'
run_entity_cli_fixture --slot 0
[ "$ENTITY_CLI_RC" = 2 ] \
  && grep -Fq -- '--slot needs a number from 1 to 999' "$cli_output" \
  && [ ! -s "$cli_mktemp_log" ] \
  && [ ! -e "$TMP/entity-cli-state/id_ed25519" ] \
  || fail 'entity naming acceptance initialized temp/key state before rejecting an invalid slot'

grep -Fq 'LIMITS_MEMORY=2GiB' "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  && grep -Fq 'NESTED_TEARDOWN_VM_MEMORY_BYTES:-2147483648' \
    "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  && grep -Fq 'NESTED_TEARDOWN_POST_LAUNCH_RESERVE_BYTES:-1073741824' \
    "$ROOT/dev/e2e/nested-teardown-data-boundary.sh" \
  || fail "nested teardown fixture can exhaust its 4 GiB allocated host"
nested_teardown_script="$ROOT/dev/e2e/nested-teardown-data-boundary.sh"
owned_backend_cleanup_line="$(grep -nF \
  'remove_owned_outer_backend || die' "$nested_teardown_script" \
  | tail -n 1 | cut -d: -f1 || true)"
host_network_assert_line="$(grep -nF \
  'assert_host_network_unchanged' "$nested_teardown_script" \
  | tail -n 1 | cut -d: -f1 || true)"
[ -n "$owned_backend_cleanup_line" ] \
  && [ -n "$host_network_assert_line" ] \
  && [ "$owned_backend_cleanup_line" -lt "$host_network_assert_line" ] \
  || fail "nested teardown checks host networking before removing its marker-owned backend"
remove_owned_outer_backend_source="$(awk '
  /^remove_owned_outer_backend\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$nested_teardown_script")"
network_cleanup_function="$TMP/remove-owned-outer-backend.sh"
printf '%s\n' "$remove_owned_outer_backend_source" > "$network_cleanup_function"
network_cleanup_bin="$TMP/nested-network-cleanup-bin"
mkdir -p "$network_cleanup_bin"
cat > "$network_cleanup_bin/incus" <<'EOF_NESTED_NETWORK_INCUS'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$NETWORK_CLEANUP_LOG"
case "$1 $2" in
  'network show') exit 0 ;;
  'network get')
    if [ "$NETWORK_CLEANUP_CASE" = foreign-owner ]; then
      printf 'foreign-owner\n'
    else
      printf 'nested-teardown-e2e-v1\n'
    fi
    ;;
  query\ *)
    case "$NETWORK_CLEANUP_CASE" in
      empty|foreign-owner) printf '{"used_by":[]}\n' ;;
      used) printf '{"used_by":["/1.0/instances/consumer"]}\n' ;;
      query-failure) exit 17 ;;
      malformed) printf '{"used_by":{}}\n' ;;
      *) exit 91 ;;
    esac
    ;;
  'network delete') exit 0 ;;
  *) exit 91 ;;
esac
EOF_NESTED_NETWORK_INCUS
chmod 0700 "$network_cleanup_bin/incus"
for network_cleanup_case in empty used foreign-owner query-failure malformed; do
  network_cleanup_log="$TMP/nested-network-cleanup-$network_cleanup_case.log"
  : > "$network_cleanup_log"
  set +e
  network_cleanup_output="$(
    PATH="$network_cleanup_bin:$PATH" \
      NETWORK_CLEANUP_CASE="$network_cleanup_case" \
      NETWORK_CLEANUP_LOG="$network_cleanup_log" \
      NETWORK_CLEANUP_FUNCTION="$network_cleanup_function" bash -c '
        set -eu
        . "$NETWORK_CLEANUP_FUNCTION"
        OUTER_POOL=""
        OUTER_BRIDGE="owned-bridge"
        if remove_owned_outer_backend; then exit 0; else exit $?; fi
      ' 2>&1
  )"
  network_cleanup_rc=$?
  set -e
  grep -Fqx 'query /1.0/networks/owned-bridge?project=default' "$network_cleanup_log" \
    || fail "nested teardown did not query the bridge API for $network_cleanup_case"
  case "$network_cleanup_case" in
    empty)
      [ "$network_cleanup_rc" = 0 ] \
        && grep -Fqx 'network delete owned-bridge --project default' "$network_cleanup_log" \
        || fail "nested teardown did not delete an empty owned network: rc=$network_cleanup_rc output=$network_cleanup_output"
      ;;
    *)
      [ "$network_cleanup_rc" != 0 ] \
        && ! grep -Fq 'network delete owned-bridge --project default' "$network_cleanup_log" \
        || fail "nested teardown deleted an unsafe $network_cleanup_case network: rc=$network_cleanup_rc output=$network_cleanup_output"
      ;;
  esac
done
nested_cleanup_function="$TMP/nested-teardown-cleanup.sh"
sed -n '/^cleanup()/,/^}$/p' "$nested_teardown_script" > "$nested_cleanup_function"
for nested_cleanup_case in success teardown-failure backend-failure; do
  nested_cleanup_state="$(mktemp -d /var/tmp/subyard-nested-teardown.cleanup.XXXXXX)"
  printf '%s\n' nested-teardown-e2e-v1 > "$nested_cleanup_state/.marker"
  set +e
  NESTED_CLEANUP_CASE="$nested_cleanup_case" \
    NESTED_CLEANUP_FUNCTION="$nested_cleanup_function" \
    NESTED_CLEANUP_STATE="$nested_cleanup_state" bash -c '
      set -eu
      . "$NESTED_CLEANUP_FUNCTION"
      yard() { [ "$NESTED_CLEANUP_CASE" != teardown-failure ]; }
      incus() { [ "$1 $2" = "project show" ]; }
      remove_owned_outer_backend() { [ "$NESTED_CLEANUP_CASE" != backend-failure ]; }
      sudo() { shift; "$@"; }
      STATE="$NESTED_CLEANUP_STATE"
      OUTER_PROJECT=owned-project
      cleanup
    ' >/dev/null 2>&1
  nested_cleanup_rc=$?
  set -e
  case "$nested_cleanup_case" in
    success)
      [ "$nested_cleanup_rc" = 0 ] && [ ! -e "$nested_cleanup_state" ] \
        || fail "nested teardown cleanup did not remove successful fixture state: rc=$nested_cleanup_rc"
      ;;
    *)
      [ "$nested_cleanup_rc" = 3 ] && [ -f "$nested_cleanup_state/.marker" ] \
        || fail "nested teardown cleanup did not retain failed $nested_cleanup_case state: rc=$nested_cleanup_rc"
      find "$nested_cleanup_state" -depth -delete
      ;;
  esac
done
nested_bridge_create_line="$(grep -nF 'incus network create "$OUTER_BRIDGE" ipv4.address=auto ipv6.address=none' \
  "$nested_teardown_script" | cut -d: -f1)"
nested_yard_init_line="$(grep -nF 'yard init --yes' "$nested_teardown_script" \
  | head -n 1 | cut -d: -f1)"
[ -n "$nested_bridge_create_line" ] && [ -n "$nested_yard_init_line" ] \
  && [ "$nested_bridge_create_line" -lt "$nested_yard_init_line" ] \
  && grep -A1 -F 'incus storage create "$OUTER_POOL" dir' "$nested_teardown_script" \
    | grep -Fq 'user.subyard.owner=nested-teardown-e2e-v1' \
  && grep -A1 -F 'incus network create "$OUTER_BRIDGE" ipv4.address=auto ipv6.address=none' \
    "$nested_teardown_script" | grep -Fq 'user.subyard.owner=nested-teardown-e2e-v1' \
  || fail 'nested teardown does not publish marker-owned pool and bridge before yard init'
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
grep -Fq 'NESTED_TEARDOWN_COMMAND_TIMEOUT:-3600' \
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
for lane in smoke boundary nested-teardown transport dependencies real-incus profile-resource release source-upgrade \
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
grep -Fq $'full\tboundary transport nested-teardown release source-upgrade power-systemd release-smoke peer cleanup' <<<"$lane_inventory" \
  || fail 'continuous P0 gate lost a mandatory public-contract phase'
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
release_update_source="$(awk '
  /^p0_apply_release_update\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/p0-guest.sh")"
release_update_mock="$TMP/release-update-yard"
release_update_log="$TMP/release-update.log"
{
  printf '%s\n' '#!/bin/sh' \
    'printf "%s\t%s\n" "${YARD_RELEASE_BASE_URL:-}" "$*" >> "$P0_RELEASE_UPDATE_LOG"'
} > "$release_update_mock"
chmod +x "$release_update_mock"
(
  eval "$release_update_source"
  P0_CURRENT_BASE_VERSION=0.8.1-p0.current-base
  P0_OWNER_VERSION=0.11.1-p0.owner
  export ROOT SUBYARD_HOME="$TMP/release-update-home"
  export P0_RELEASE_UPDATE_LOG="$release_update_log"
  p0_apply_release_update "$release_update_mock" "$P0_OWNER_VERSION"
  p0_apply_release_update "$release_update_mock" "$P0_CURRENT_BASE_VERSION"
)
set +e
(
  eval "$release_update_source"
  P0_CURRENT_BASE_VERSION=0.8.1-p0.current-base
  P0_OWNER_VERSION=0.11.1-p0.owner
  export ROOT SUBYARD_HOME="$TMP/release-update-home"
  export P0_RELEASE_UPDATE_LOG="$release_update_log"
  die() { exit 2; }
  p0_apply_release_update "$release_update_mock" 0.11.1-p0.foreign
) >/dev/null 2>&1
unsupported_release_rc=$?
set -e
[ "$unsupported_release_rc" = 2 ] && [ "$(wc -l < "$release_update_log")" = 4 ] \
  || fail 'P0 release-impact update accepts an unowned synthetic release'
[ "$(cat "$release_update_log")" = "$(printf '%s\t%s\n' \
  "file://$ROOT/.build/p0-owner-release" \
  "update --runtime-root $TMP/release-update-home/runtime --version 0.11.1-p0.owner --check" \
  "file://$ROOT/.build/p0-owner-release" \
  "update --runtime-root $TMP/release-update-home/runtime --version 0.11.1-p0.owner --yes" \
  "file://$ROOT/.build/p0-current-base-release" \
  "update --runtime-root $TMP/release-update-home/runtime --version 0.8.1-p0.current-base --check" \
  "file://$ROOT/.build/p0-current-base-release" \
  "update --runtime-root $TMP/release-update-home/runtime --version 0.8.1-p0.current-base --yes")" ] \
  || fail 'P0 release-impact update does not use its exact local synthetic release'
owner_capacity_reclaim_source="$(awk '
  /^reclaim_owner_lease_capacity\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/p0-guest.sh")"
! grep -Fq '"$ROOT/.build/p0-owner-release"' <<<"$owner_capacity_reclaim_source" \
  || fail 'P0 owner capacity reclaim deletes a release artifact used by later updates'
p0_incus_root="$TMP/p0-incus-bootstrap/platform"
p0_incus_storage="$p0_incus_root/incus/incus/storage"
owner_incus_call="$TMP/p0-owner-incus-call"
(
  P0_CAPACITY_PLATFORM_ROOT="$p0_incus_root"
  p0_capacity_prepare_platform_root() { :; }
  ensure_incus() { printf '%s\n' "$*" > "$owner_incus_call"; }
  reconcile_p0_incus_apparmor_compat() { :; }
  eval "$(sed -n '/^ensure_owner_incus() {/,/^}/p' "$ROOT/dev/e2e/p0-guest.sh")"
  ensure_owner_incus owner
) || fail 'P0 owner Incus bootstrap path selection failed'
[ "$(cat "$owner_incus_call")" = "$p0_incus_root  owner $p0_incus_storage" ] \
  || fail 'P0 owner Incus bootstrap lost storage separation'
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
grep -Fq 'mapfile -t SLOT_IDS' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'prepare_slot "$TARGET_SLOT"' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'prepare_slot "$PEER_SLOT"' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'LEASE_PURPOSE=acceptance-prepare' \
    "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'acquire_lease' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'guest "$vm" sh -eu -c' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'fstrim -av' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'assert_untouched_slots' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'dump_broker_diagnostics' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail "P1 acceptance does not discover and prepare target slots with neighbor diagnostics"
! grep -Fq '(.pool.slots | length) == 2' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq -- '--wait 2s' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'owner=$A_YARD/$A_PROJECT run=$A_RUN purpose=holder-a' \
    "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail 'P1 acceptance still assumes two slots or omits bounded owner-aware wait coverage'
(
  export OWNER_YARD_DIR="$TMP/owner-registration/yards"
  export MARKER=subyard-p0-owner-registration-test
  export OWNER_DIAGNOSTIC_DEV_UID=1001
  export OWNER_DIAGNOSTIC_VM_MEMORY=700MiB
  export OWNER_DIAGNOSTIC_VM_BOOT_TIMEOUT=600
  export OWNER_BASE_IMAGE=subyard-e2e-test-image
  die() { exit 2; }
  eval "$(sed -n '/^write_owner_registration() {/,/^}/p' \
    "$ROOT/dev/e2e/p0-guest.sh")"
  umask 022
  write_owner_registration test-yard test-vms 2224
  [ "$(stat -c %a "$OWNER_YARD_DIR/test-yard.env")" = 600 ] || exit 1
  grep -Fxq AGENTS=none "$OWNER_YARD_DIR/test-yard.env" || exit 1
  mkdir "$OWNER_YARD_DIR/test-yard"
  mv "$OWNER_YARD_DIR/test-yard.env" "$OWNER_YARD_DIR/test-yard/config.env"
  write_owner_registration test-yard test-vms 2224 3
  [ ! -e "$OWNER_YARD_DIR/test-yard.env" ] || exit 1
  [ "$(stat -c %a "$OWNER_YARD_DIR/test-yard/config.env")" = 600 ] || exit 1
  grep -Fxq E2E_VM_SLOT_COUNT=3 "$OWNER_YARD_DIR/test-yard/config.env" || exit 1
  grep -Fxq "CODING_TOOL_INTEGRATIONS=''" "$OWNER_YARD_DIR/test-yard/config.env" || exit 1
  ! grep -q '^AGENTS=' "$OWNER_YARD_DIR/test-yard/config.env" || exit 1
  printf '# unrelated\n' > "$OWNER_YARD_DIR/test-yard/config.env"
  ! (write_owner_registration test-yard test-vms 2224 2) || exit 1
  [ "$(cat "$OWNER_YARD_DIR/test-yard/config.env")" = '# unrelated' ]
) || fail 'P0 owner registration did not preserve private, owned legacy/canonical fixtures'
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
owner_image_setup_source="$(awk '
  /^owner\(\) \(/ { in_owner=1 }
  in_owner && /^[[:space:]]*ensure_owner_incus$/ { capture=1 }
  capture { print }
  capture && /OWNER_BASELINE_CAPTURED=1/ { exit }
' "$ROOT/dev/e2e/p0-guest.sh")"
owner_cleanup_source="$(sed -n '/^owner_cleanup() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-guest.sh")"
owner_image_state="$TMP/owner-images"
owner_image_deletes="$TMP/owner-image-deletes"
printf '%s\n' base-original > "$owner_image_state"
mkdir -p "$TMP/owner-yards"
set +e
OWNER_IMAGE_SETUP_SOURCE="$owner_image_setup_source" \
  OWNER_CLEANUP_SOURCE="$owner_cleanup_source" \
  OWNER_IMAGE_STATE="$owner_image_state" \
  OWNER_IMAGE_DELETES="$owner_image_deletes" \
  OWNER_YARD_DIR="$TMP/owner-yards" \
  OWNER_ROOT="$TMP/absent-owner-root" \
  SUBYARD_HOME="$TMP/absent-owner-home" bash -c '
    set -euo pipefail
    eval "$OWNER_CLEANUP_SOURCE"
    TOKEN=images
    MARKER=owner-image-fixture
    OWNER_BASELINE_IMAGES=
    OWNER_BASELINE_CAPTURED=0
    clean_tree() { :; }
    ensure_owner_incus() { :; }
    reclaim_owner_project_if_present() { :; }
    cleanup_owner_capacity_state() { :; }
    bash() {
      [ "$*" = dev/e2e/p0-real-incus.sh ] || return 90
      printf "%s\n" shared-prepared >> "$OWNER_IMAGE_STATE"
    }
    incus() {
      case "$1 $2" in
        "image list") cat "$OWNER_IMAGE_STATE" ;;
        "image delete")
          printf "%s\n" "$3" >> "$OWNER_IMAGE_DELETES"
          grep -Fxv -- "$3" "$OWNER_IMAGE_STATE" > "$OWNER_IMAGE_STATE.next"
          mv "$OWNER_IMAGE_STATE.next" "$OWNER_IMAGE_STATE"
          ;;
        *) return 91 ;;
      esac
    }
    eval "$OWNER_IMAGE_SETUP_SOURCE"
    [ "$OWNER_BASELINE_CAPTURED" = 1 ]
    printf "%s\n" owner-ephemeral >> "$OWNER_IMAGE_STATE"
    owner_cleanup
  '
owner_image_cleanup_rc=$?
set -e
[ "$owner_image_cleanup_rc" = 0 ] \
  && [ "$(cat "$owner_image_state")" = $'base-original\nshared-prepared' ] \
  && [ "$(cat "$owner_image_deletes")" = owner-ephemeral ] \
  || fail 'P0 owner cleanup removed a prepared shared image or retained an owner image'
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
  && grep -Fq 'source_upgrade_shared_root_tokens' "$ROOT/dev/e2e/p0-guest.sh" \
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
  local fixture_active="${5:-0}" shared_root_tokens="${6:-}"
  SOURCE_RECOVERY_INVENTORY="$inventory" \
  SOURCE_RECOVERY_E2E_MARKER="$e2e_marker" \
  SOURCE_RECOVERY_TEST_MARKER="$test_marker" \
  SOURCE_RECOVERY_DEFAULT_MARKER="$default_marker" \
  SOURCE_RECOVERY_FIXTURE_ACTIVE="$fixture_active" \
  SOURCE_RECOVERY_SHARED_ROOT_TOKENS="$shared_root_tokens" \
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
    source_upgrade_shared_root_tokens() {
      printf "%s\n" "$SOURCE_RECOVERY_SHARED_ROOT_TOKENS"
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
[ "$(run_source_recovery_case subyard-test-yard '' '' '' 0 441 \
  | tail -n 1)" = 441 ] \
  || fail 'P0 source fixture recovery missed a markerless migrated project'
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
set +e
source_recovery_failure="$(run_source_recovery_case \
  subyard-test-yard '' subyard-p0-source-441 '' 0 442 2>&1)"
source_recovery_rc=$?
set -e
[ "$source_recovery_rc" = 2 ] \
  && grep -Fq 'conflicting fixture markers' <<<"$source_recovery_failure" \
  || fail 'P0 source fixture recovery combined conflicting project and durable markers'
real_incus_recovery_function="$(awk '
  /^recover_stale_real_incus_fixture\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/p0-guest.sh")"
run_real_incus_recovery_case() {
  ROOT="$ROOT" REAL_INCUS_RECOVERY_CASE="$1" \
  REAL_INCUS_RECOVERY_FUNCTION="$real_incus_recovery_function" bash -c '
    set -euo pipefail
    SUBYARD_E2E_VM=1
    project_present=1
    cleanup_calls=0
    die() { printf "%s\\n" "$*" >&2; exit 2; }
    command() {
      [ "$1 ${2:-}" != "-v incus" ] || return 0
      builtin command "$@"
    }
    incus() {
      [ "$1 $2" = "project list" ] || exit 93
      [ "$REAL_INCUS_RECOVERY_CASE" != initial-query-error ] || return 17
      [ "$project_present" = 0 ] || printf "%s\\n" subyard-p0-real-incus
    }
    pgrep() {
      case "$REAL_INCUS_RECOVERY_CASE" in
        active) return 0 ;;
        query-error) return 2 ;;
        *) return 1 ;;
      esac
    }
    bash() {
      [ "$1" = "$ROOT/dev/e2e/p0-real-incus.sh" ] && [ "$2" = --cleanup-only ] \
        || exit 91
      cleanup_calls=$((cleanup_calls + 1))
      [ "$REAL_INCUS_RECOVERY_CASE" != remains ] || return 0
      project_present=0
    }
    timeout() {
      case "$1" in
        --foreground) shift 2 ;;
        --signal=TERM) shift 3 ;;
        *) exit 92 ;;
      esac
      "$@"
    }
    eval "$REAL_INCUS_RECOVERY_FUNCTION"
    case "$REAL_INCUS_RECOVERY_CASE" in absent) project_present=0 ;; esac
    recover_stale_real_incus_fixture
    printf "%s\\n" "$cleanup_calls"
  '
}
[ "$(run_real_incus_recovery_case absent)" = 0 ] \
  || fail 'P0 preflight recovered a missing real-Incus project'
[ "$(run_real_incus_recovery_case stale | tail -n 1)" = 1 ] \
  || fail 'P0 preflight did not reclaim the exact stale real-Incus project'
for real_incus_recovery_case in active query-error remains initial-query-error; do
  set +e
  real_incus_recovery_output="$(run_real_incus_recovery_case "$real_incus_recovery_case" 2>&1)"
  real_incus_recovery_rc=$?
  set -e
  [ "$real_incus_recovery_rc" = 2 ] \
    || fail "P0 preflight accepted a $real_incus_recovery_case real-Incus fixture"
  case "$real_incus_recovery_case" in
    active) grep -Fq 'active process' <<<"$real_incus_recovery_output" ;;
    query-error) grep -Fq 'cannot determine whether' <<<"$real_incus_recovery_output" ;;
    remains) grep -Fq 'remains after recovery' <<<"$real_incus_recovery_output" ;;
    initial-query-error) grep -Fq 'cannot inspect real-Incus fixture inventory' <<<"$real_incus_recovery_output" ;;
  esac || fail "P0 preflight did not fail closed for $real_incus_recovery_case"
done
sed -n '/^capacity_preflight() {/,/^}/p' "$ROOT/dev/e2e/p0-guest.sh" \
  | tr '\n' ' ' \
  | grep -Eq 'recover_stale_source_upgrade_fixture.*recover_stale_real_incus_fixture.*recover_stale_test_default_pool' \
  || fail 'P0 real-Incus recovery is not ordered before pool and capacity checks'
grep -Fq 'is_markerless_migrated_fixture_project' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'user.subyard.test_vms_revision' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'refusing unmarked Incus project' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail 'source-upgrade cleanup cannot prove a markerless migrated fixture'
markerless_source_function="$(awk '
  /^is_markerless_migrated_fixture_project\(\)/ { copying=1 }
  copying { print }
  copying && /^}$/ { exit }
' "$ROOT/dev/e2e/p0-source-upgrade.sh")"
run_markerless_source_case() {
  local initialized="$1" revision="$2" name="$3"
  SOURCE_MARKERLESS_INITIALIZED="$initialized" \
  SOURCE_MARKERLESS_REVISION="$revision" \
  SOURCE_MARKERLESS_ROOT="$TMP/markerless-source-$name" \
  SOURCE_MARKERLESS_FUNCTION="$markerless_source_function" bash -c '
    set -euo pipefail
    OPERATOR="$(id -un)"
    OPERATOR_HOME="$SOURCE_MARKERLESS_ROOT/home"
    OPERATOR_HOME_MARKER="$OPERATOR_HOME/.subyard-p0-source-home"
    SHARED_ROOT="$SOURCE_MARKERLESS_ROOT/shared"
    MARKER=subyard-p0-source-441
    registration="$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env"
    mkdir -p "$SHARED_ROOT" "$(dirname "$registration")"
    printf "%s\n" "$MARKER" > "$SHARED_ROOT/.subyard-p0-marker"
    printf "%s\n" "$MARKER" > "$OPERATOR_HOME_MARKER"
    printf "%s\n" YARD_TEMPLATE=test-vms > "$registration"
    sudo() {
      [ "${1:-}" != -n ] || shift
      "$@"
    }
    incus() {
      case "$*" in
        "project get subyard-test-yard restricted") printf "%s\n" true ;;
        "project get subyard-test-yard features.images") printf "%s\n" false ;;
        "list --project subyard-test-yard --format csv -c n")
          printf "%s\n" yard-test-yard
          ;;
        "storage volume list default --project subyard-test-yard --format csv -c t,n")
          printf "%s\n" container,yard-test-yard custom,yard-srv-test-yard
          ;;
        "config show yard-test-yard --project subyard-test-yard") return 0 ;;
        "config get yard-test-yard user.subyard.managed --project subyard-test-yard")
          printf "%s\n" true
          ;;
        "config get yard-test-yard user.subyard.name --project subyard-test-yard")
          printf "%s\n" test-yard
          ;;
        "config get yard-test-yard user.subyard.initialized --project subyard-test-yard")
          printf "%s\n" "$SOURCE_MARKERLESS_INITIALIZED"
          ;;
        "config get yard-test-yard user.subyard.test_vms_revision --project subyard-test-yard")
          printf "%s\n" "$SOURCE_MARKERLESS_REVISION"
          ;;
        *) return 3 ;;
      esac
    }
    eval "$SOURCE_MARKERLESS_FUNCTION"
    is_markerless_migrated_fixture_project subyard-test-yard
  '
}
run_markerless_source_case true 1:fixture:test-yard complete \
  || fail 'source-upgrade cleanup rejected a complete markerless migrated fixture'
run_markerless_source_case false '' interrupted \
  || fail 'source-upgrade cleanup rejected its exact pre-init markerless migrated fixture'
if run_markerless_source_case false 1:fixture:test-yard conflicting >/dev/null 2>&1; then
  fail 'source-upgrade cleanup accepted an initialized/revision conflict'
fi
if run_markerless_source_case true '' incomplete >/dev/null 2>&1; then
  fail 'source-upgrade cleanup accepted an initialized fixture without broker revision'
fi
grep -Fq 'cold Go dependency download heartbeat elapsed=' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'dependency download failed (attempt %s/3); retrying in %ss' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'timeout --signal=TERM --kill-after=10' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'dependency bootstrap lacks bounded retry and heartbeat progress'
grep -Fq 'p0_capacity_query_default_pool state "$query_timeout"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'Incus default-pool query exceeded' "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  && grep -Fq 'retrying cold activation' "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  && grep -Fq 'timeout --foreground "$query_timeout"' \
    "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  || fail 'P0 capacity preflight can hang on a partial Incus daemon'
grep -Fq 'Environment=INCUS_SECURITY_APPARMOR=false' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'reconcile_p0_incus_apparmor_compat' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'restore_p0_incus_apparmor_default' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'refusing foreign Incus compatibility drop-in' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'P0 outer Incus AppArmor compatibility is not marker-owned and reversible'
grep -Fq 'p0_capacity_recover_stale_roots' "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  && grep -Fq 'stale P0 state still has an active process' \
    "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  || fail 'P0 preflight cannot distinguish orphaned from active marker-owned cache state'
grep -Fq 'OWNER_DIAGNOSTIC_DEV_UID="${P0_E2E_DIAGNOSTIC_DEV_UID:-1001}"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'chown -R "$OWNER_DIAGNOSTIC_DEV_UID:$OWNER_DIAGNOSTIC_DEV_UID" "$bound"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 bind fixture is not owned by its configured diagnostic yard UID"
owner_sink_cleanup_function="$(sed -n '/^cleanup_owner_test_vms_sink() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-guest.sh")"
owner_capacity_cleanup_function="$(sed -n '/^cleanup_owner_capacity_state() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-guest.sh")"
for owner_sink_scenario in success absent timer-failure service-failure foreign-service foreign-timer missing-service malformed-command missing-home duplicate-home wrong-owner symlink; do
  owner_sink_fixture="$TMP/owner-sink-$owner_sink_scenario"
  install -d -m 0700 "$owner_sink_fixture"
  OWNER_SINK_SCENARIO="$owner_sink_scenario" OWNER_SINK_FIXTURE="$owner_sink_fixture" \
    OWNER_SINK_CLEANUP_FUNCTION="$owner_sink_cleanup_function" \
    OWNER_CAPACITY_CLEANUP_FUNCTION="$owner_capacity_cleanup_function" \
    OWNER_SINK_TIMER_TEMPLATE="$ROOT/config/systemd/subyard-test-vms-host-sink.timer.in" bash -c '
      set -euo pipefail
      OWNER_ROOT="$OWNER_SINK_FIXTURE/state"
      OWNER_DATA_ROOT="$OWNER_ROOT/subyard"
      OWNER_TEST_VMS_SINK="$OWNER_SINK_FIXTURE/test-vms-host-sink"
      OWNER_TEST_VMS_SINK_SERVICE_NAME=subyard-test-vms-host-sink.service
      OWNER_TEST_VMS_SINK_SERVICE="$OWNER_SINK_FIXTURE/$OWNER_TEST_VMS_SINK_SERVICE_NAME"
      OWNER_TEST_VMS_SINK_TIMER_NAME=subyard-test-vms-host-sink.timer
      OWNER_TEST_VMS_SINK_TIMER="$OWNER_SINK_FIXTURE/$OWNER_TEST_VMS_SINK_TIMER_NAME"
      OWNER_TEST_VMS_SINK_TIMER_TEMPLATE="$OWNER_SINK_TIMER_TEMPLATE"
      OWNER_SINK_LOG="$OWNER_SINK_FIXTURE/calls"
      install -d -m 0700 "$OWNER_DATA_ROOT"
      case "$OWNER_SINK_SCENARIO" in
        absent) ;;
        missing-service) cp "$OWNER_TEST_VMS_SINK_TIMER_TEMPLATE" "$OWNER_TEST_VMS_SINK_TIMER" ;;
        *)
          : > "$OWNER_TEST_VMS_SINK"
          cp "$OWNER_TEST_VMS_SINK_TIMER_TEMPLATE" "$OWNER_TEST_VMS_SINK_TIMER"
          printf "%s\n" \
            "Environment=\"SUBYARD_HOME=$OWNER_DATA_ROOT\"" \
            "ExecStart=$OWNER_TEST_VMS_SINK _test-vms-host-sink sync" \
            > "$OWNER_TEST_VMS_SINK_SERVICE"
          ;;
      esac
      if [ "$OWNER_SINK_SCENARIO" = foreign-service ]; then
        sed -i "s|$OWNER_DATA_ROOT|$OWNER_SINK_FIXTURE/foreign|" \
          "$OWNER_TEST_VMS_SINK_SERVICE"
      fi
      if [ "$OWNER_SINK_SCENARIO" = foreign-timer ]; then
        printf "\n[Timer]\nUnit=unrelated.service\n" >> "$OWNER_TEST_VMS_SINK_TIMER"
      fi
      case "$OWNER_SINK_SCENARIO" in
        malformed-command) sed -i "s/ sync$/ foreign/" "$OWNER_TEST_VMS_SINK_SERVICE" ;;
        missing-home) sed -i "/^Environment=/d" "$OWNER_TEST_VMS_SINK_SERVICE" ;;
        duplicate-home) printf "Environment=\"SUBYARD_HOME=/foreign\"\n" >> "$OWNER_TEST_VMS_SINK_SERVICE" ;;
        symlink) mv "$OWNER_TEST_VMS_SINK" "$OWNER_SINK_FIXTURE/real-sink"; ln -s real-sink "$OWNER_TEST_VMS_SINK" ;;
      esac
      stat() {
        if [ "$OWNER_SINK_SCENARIO" = wrong-owner ]; then printf "1000:1000\n"; else printf "0:0\n"; fi
      }
      sudo() {
        printf "%s\n" "$*" >> "$OWNER_SINK_LOG"
        case "$*" in
          "-n systemctl disable --now $OWNER_TEST_VMS_SINK_TIMER_NAME")
            [ "$OWNER_SINK_SCENARIO" != timer-failure ]
            ;;
          "-n systemctl stop $OWNER_TEST_VMS_SINK_SERVICE_NAME")
            [ "$OWNER_SINK_SCENARIO" != service-failure ]
            ;;
          "-n systemctl daemon-reload") ;;
          "-n find "*) shift 2; command find "$@" ;;
          *) return 97 ;;
        esac
      }
      p0_capacity_remove_subtree() { printf "remove-subtree\n" >> "$OWNER_SINK_LOG"; }
      p0_capacity_remove_build_cache() { printf "remove-build-cache\n" >> "$OWNER_SINK_LOG"; }
      p0_capacity_remove_root_if_empty() { printf "remove-root\n" >> "$OWNER_SINK_LOG"; }
      eval "$OWNER_SINK_CLEANUP_FUNCTION"
      eval "$OWNER_CAPACITY_CLEANUP_FUNCTION"
      set +e
      cleanup_owner_capacity_state 2> "$OWNER_SINK_FIXTURE/stderr"
      cleanup_rc=$?
      set -e
      case "$OWNER_SINK_SCENARIO" in
        success)
          [ "$cleanup_rc" = 0 ]
          [ ! -e "$OWNER_TEST_VMS_SINK" ] \
            && [ ! -e "$OWNER_TEST_VMS_SINK_SERVICE" ] \
            && [ ! -e "$OWNER_TEST_VMS_SINK_TIMER" ]
          [ "$(grep -c "^remove-" "$OWNER_SINK_LOG")" = 3 ]
          ;;
        absent)
          [ "$cleanup_rc" = 0 ]
          [ "$(grep -c "^remove-" "$OWNER_SINK_LOG")" = 3 ]
          ;;
        foreign-service)
          [ "$cleanup_rc" = 0 ]
          [ -f "$OWNER_TEST_VMS_SINK" ] \
            && [ -f "$OWNER_TEST_VMS_SINK_SERVICE" ] \
            && [ -f "$OWNER_TEST_VMS_SINK_TIMER" ]
          ! grep -q "^-n " "$OWNER_SINK_LOG"
          [ "$(grep -c "^remove-" "$OWNER_SINK_LOG")" = 3 ]
          ;;
        *)
          [ "$cleanup_rc" -ne 0 ]
          ! grep -q "^remove-" "$OWNER_SINK_LOG" 2>/dev/null
          [ -d "$OWNER_ROOT" ]
          case "$OWNER_SINK_SCENARIO" in
            foreign-timer|missing-service|malformed-command|missing-home|duplicate-home|wrong-owner|symlink)
              ! grep -q "^-n " "$OWNER_SINK_LOG" 2>/dev/null ;;
          esac
          ;;
      esac
    ' || fail "P0 owner sink cleanup violated $owner_sink_scenario isolation"
done
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
remove_reclaim_fixture_source="$(sed -n \
  '/^remove_reclaim_fixture() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh")"
reclaim_cleanup_attempts="$TMP/reclaim-cleanup-attempts"
: > "$reclaim_cleanup_attempts"
set +e
REMOVE_RECLAIM_FIXTURE_SOURCE="$remove_reclaim_fixture_source" \
  RECLAIM_CLEANUP_ATTEMPTS="$reclaim_cleanup_attempts" bash -c '
    set -u
    eval "$REMOVE_RECLAIM_FIXTURE_SOURCE"
    RECLAIM_MARKER=subyard-p0-release-reclaim-v1:1234abcd
    RECLAIM_FIXTURE=/var/tmp/subyard-p0-release-reclaim
    ssh() {
      target=
      for argument do
        case "$argument" in e2e-vm-[12]) target=$argument ;; esac
      done
      printf "%s\n" "$target" >> "$RECLAIM_CLEANUP_ATTEMPTS"
      [ "$target" != e2e-vm-1 ]
    }
    remove_reclaim_fixture fixture-config
  '
reclaim_cleanup_rc=$?
set -e
[ "$reclaim_cleanup_rc" -ne 0 ] \
  && [ "$(cat "$reclaim_cleanup_attempts")" = $'e2e-vm-1\ne2e-vm-2' ] \
  || fail "reclaim cleanup did not preserve VM1 failure while attempting VM2: rc=$reclaim_cleanup_rc"
awk '
  /^RECLAIM_MARKER="subyard-p0-release-reclaim-v1:\$neighbor_run"$/ { armed = NR }
  /^stage_reclaim_fixture "\$NEIGHBOR_CONFIG" 1$/ { staged = NR }
  END { exit !(armed && staged && armed + 1 == staged) }
' "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  || fail 'reclaim cleanup marker is armed before a fixture can exist'
grep -Fq '"$runtime_root/current/bin/yard" update \' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && ! grep -Fq 'scripts/install-runtime-release.sh' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  || fail 'P0 broker recovery bypasses the release-transition owner'
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
grep -Fq 'start_holder_child hold_lease victim quarantine-victim slot-001' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'start_holder_child hold_lease neighbor held-neighbor slot-002' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'stop_holder_child "$VICTIM_PID"' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'stop_holder_child "$NEIGHBOR_PID"' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  || fail 'P0 broker recovery readiness timeout bypasses bounded holder shutdown'
(
  holder_pid=''
  trap '
    [ -z "$holder_pid" ] || kill -KILL -- "-$holder_pid" >/dev/null 2>&1 || true
  ' EXIT
  eval "$(sed -n '/^recovery_monotonic_seconds() {/,/^}/p' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh")"
  eval "$(sed -n '/^start_holder_child() {/,/^}/p' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh")"
  eval "$(sed -n '/^stop_holder_child() {/,/^}/p' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh")"
  # Consumed by the dynamically extracted helper.
  # shellcheck disable=SC2034
  HOLDER_STOP_GRACE_SECONDS=1
  # Consumed by the dynamically extracted helper.
  # shellcheck disable=SC2034
  HOLDER_KILL_GRACE_SECONDS=1
  start_holder_child bash -c '
    trap "" TERM
    (trap "" TERM; while :; do sleep 10; done) &
    while :; do sleep 10; done
  '
  holder_pid="$HOLDER_STARTED_PID"
  started=$SECONDS
  stop_holder_child "$holder_pid" 2>/dev/null
  [ "$((SECONDS - started))" -le 4 ] \
    && ! kill -0 -- "-$holder_pid" >/dev/null 2>&1
) || fail 'P0 broker recovery readiness timeout does not bound holder process-group shutdown'
if grep -Fq 'mask --runtime --now incus.service' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh"; then
  fail "P0 broker recovery fault injection drains unrelated held leases"
fi
grep -Fq '> "$PEER_ROOT/config/config.env"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'P0_PEER_YARD_TIMEOUT:-1800' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer yard does not use its active config root with a bounded init"
grep -Fq 'SUBYARD_SOURCE_INGRESS_V1_ROOT' \
  "$ROOT/dev/bootstrap-runtime.sh" \
  && ! grep -Fq '_release-transition' "$ROOT/scripts/migrate-source-install.sh" \
  && ! grep -Eq '_migrate[[:space:]]+(apply|finalize|rollback|cleanup)' \
    "$ROOT/scripts/migrate-source-install.sh" \
  && grep -Fq '[ ! -e "$recovery_root/source-plan" ]' \
    "$ROOT/scripts/migrate-source-install.sh" \
  && ! grep -Fq '_migrate-test-yard' "$ROOT/dev/bootstrap-runtime.sh" \
  || fail "source bootstrap does not use the unified release transition lifecycle"
grep -Fq 'export SUBYARD_POWER_RECONCILER_PATH="$TMP/missing-power-reconciler"' \
  "$ROOT/tests/engine-release.sh" \
  && grep -Fq 'export SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit"' \
    "$ROOT/tests/engine-release.sh" \
  || fail "host-free engine release can observe the physical host power reconciler"
v0111_recovery="$ROOT/dev/e2e/release-transition-v0111-recovery.sh"
v0111_observer="$ROOT/dev/e2e/release-transition-post-cas-observer.py"
grep -Fq 'OLD_VERSION=0.9.1' "$v0111_recovery" \
  && grep -Fq 'SOURCE_VERSION=0.11.1' "$v0111_recovery" \
  && grep -Fq 'CANDIDATE_VERSION=0.11.2' "$v0111_recovery" \
  && grep -Fq '5bd3c61e3dd39cb2d258be5cd75237383f00eff0512c77a3a5ca75d96e6b992b' \
    "$v0111_recovery" \
  && grep -Fq '41acb799e55cf82cdbbef8e7f75e8e17c2df344c1ceeb748181fb6aec7ea6f8d' \
    "$v0111_recovery" \
  && [ "$(grep -Fc 'https://github.com/Subyard/Subyard/releases/download/v$version/subyard-install.sh' \
    "$v0111_recovery")" -eq 1 ] \
  || fail 'v0.11.1 recovery fixture does not bind both official predecessor installers'
v0111_fixture_env="$(bash -c '
  set -euo pipefail
  source "$1"
  TOKEN=123
  STATE_ROOT="$2"
  SUCCESS_ROOT="$STATE_ROOT/success"
  SUCCESS_YARD=v0111-success-123
  MARKER=subyard-p0-v0111-123
  select_fixture success
  fixture_env sh -c '\''printf "%s|%s|%s|%s\n" \
    "${SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE:-}" \
    "${SUBYARD_TEST_VMS_SINK_PATH:-}" \
    "${SUBYARD_TEST_VMS_SINK_SERVICE_PATH:-}" \
    "${SUBYARD_TEST_VMS_SINK_TIMER_PATH:-}"'\''
' _ "$v0111_recovery" "$TMP/v0111-env")"
[ "$v0111_fixture_env" = \
  '1|/usr/local/libexec/subyard/subyard-p0-v0111-success-123-test-vms-sink|/etc/systemd/system/subyard-p0-v0111-success-123-test-vms-host-sink.service|/etc/systemd/system/subyard-p0-v0111-success-123-test-vms-host-sink.timer' ] \
  || fail 'v0.11.1 fixture does not isolate optional user timers and the root host sink'
bash -c '
  set -euo pipefail
  source "$1"
  TOKEN=123
  STATE_ROOT="$2"
  SUCCESS_ROOT="$STATE_ROOT/success"
  SUCCESS_YARD=v0111-success-123
  select_fixture success
  write_fixture_config
  registration="$CONFIG_HOME/yards/$YARD_NAME/config.env"
  grep -Fxq YARD_KIND=container "$registration"
  grep -Fxq AGENTS= "$registration"
  ! grep -Eq "^(YARD_TEMPLATE|NESTED_E2E_VMS|E2E_VM_)" "$registration"
' _ "$v0111_recovery" "$TMP/v0111-config" \
  || fail 'v0.11.1 config-drift fixture must allow the selected Codex integration'
grep -Fq 'sed -i '\''s/^AGENTS=$/AGENTS=codex/'\'' "$CONFIG_HOME/yards/$YARD_NAME/config.env"' \
    "$v0111_recovery" \
  && grep -Fq 'assert_source_dead_end' "$v0111_recovery" \
  && grep -Fq '.blockers[0].resource == "transition.observation-scope"' "$v0111_recovery" \
  && grep -Fq 'SUBYARD_YARD="$YARD_NAME"' "$v0111_recovery" \
  && ! grep -Fq 'release-transition-fixture' "$v0111_recovery" \
  && ! grep -Fq 'MarshalJournal' "$v0111_recovery" \
  || fail 'real v0.11.1 fixture is synthetic or omits the materialized-config dead-end'
grep -Fq 'SUCCESS_ROOT="$STATE_ROOT/success"' "$v0111_recovery" \
  && grep -Fq 'POST_CAS_ROOT="$STATE_ROOT/post-cas"' "$v0111_recovery" \
  && grep -Fq 'v0111-success-$TOKEN' "$v0111_recovery" \
  && grep -Fq 'v0111-post-cas-$TOKEN' "$v0111_recovery" \
  && [ "$(grep -Fc 'reproduce_official_dead_end' "$v0111_recovery")" -ge 3 ] \
  && ! grep -Fq 'copy_dead_end_for_crash' "$v0111_recovery" \
  || fail 'success and post-CAS fixtures are not independent official histories'
grep -Fq 'setsid env' "$v0111_recovery" \
  && grep -Fq 'python3 "$POST_CAS_OBSERVER"' "$v0111_recovery" \
  && grep -Fq 'POST_CAS_OBSERVATION_MARKER=' "$v0111_recovery" \
  && grep -Fq 'test ! -e "$1" && test ! -L "$1"' "$v0111_recovery" \
  && grep -Fq 'wait "$POST_CAS_TRANSITION_PID"' "$v0111_recovery" \
  && grep -Fq '.transaction != $source and .checkpoint != "complete"' "$v0111_recovery" \
  && grep -Fq '[ "$transition_rc" = 137 ]' "$v0111_recovery" \
  && grep -Fq 'ordinary post-CAS resume requested a second confirmation' "$v0111_recovery" \
  && ! grep -Fq 'P0_V0111_REAL_INCUS' "$v0111_recovery" \
  || fail 'v0.11.1 recovery fixture omits exact post-CAS interruption and ordinary resume'
[ -f "$v0111_observer" ] \
  && grep -Fq 'inotify_init1' "$v0111_observer" \
  && grep -Fq 'IN_MOVED_TO' "$v0111_observer" \
  && grep -Fq 'os.O_NOFOLLOW' "$v0111_observer" \
  && grep -Fq 'os.getpid() != os.getpgrp()' "$v0111_observer" \
  && grep -Fq 'os.getpgid(child.pid) != os.getpgrp()' "$v0111_observer" \
  && grep -Fq '"activation-intent", "target-active", "reconciling"' "$v0111_observer" \
  && grep -Fq 'journal.get("goal", {}).get("target") != args.candidate_target' \
    "$v0111_observer" \
  && grep -Fq 'kill_isolated_group(signal.SIGKILL)' "$v0111_observer" \
  && grep -Fq 'os.killpg(os.getpgrp(), signum)' "$v0111_observer" \
  || fail 'v0.11.1 post-CAS observer does not bind the atomic link event to SIGKILL'
v0111_observer_probe="$TMP/v0111-post-cas-observer"
mkdir -p "$v0111_observer_probe/runtime/releases/source" \
  "$v0111_observer_probe/runtime/releases/0.11.2-candidate"
ln -s releases/source "$v0111_observer_probe/runtime/current"
printf '{"transaction":"source","checkpoint":"reconciling"}\n' \
  > "$v0111_observer_probe/journal.json"
v0111_observer_rc=0
setsid python3 "$v0111_observer" \
  --runtime-root "$v0111_observer_probe/runtime" \
  --journal "$v0111_observer_probe/journal.json" \
  --source-transaction source --candidate-target 0.11.2-candidate \
  --marker "$v0111_observer_probe/observed.json" --timeout 5 -- \
  sh -c '
    sleep 0.05
    printf "{\"transaction\":\"candidate\",\"checkpoint\":\"activation-intent\",\"goal\":{\"target\":\"0.11.2-candidate\",\"direction\":\"activate-target\"},\"releases\":{\"from\":\"source\",\"target\":\"0.11.2-candidate\"}}\\n" \
      > "$1/journal.next"
    mv "$1/journal.next" "$1/journal.json"
    ln -s releases/0.11.2-candidate "$1/runtime/current.next"
    mv -T "$1/runtime/current.next" "$1/runtime/current"
    sleep 5
  ' _ "$v0111_observer_probe" \
  >/dev/null 2>"$v0111_observer_probe/stderr" &
v0111_observer_pid=$!
if wait "$v0111_observer_pid"; then
  v0111_observer_rc=0
else
  v0111_observer_rc=$?
fi
[ "$v0111_observer_rc" = 137 ] \
  && [ "$(stat -c '%a' "$v0111_observer_probe/observed.json")" = 600 ] \
  && jq -e '.transaction == "candidate" and
    .checkpoint == "activation-intent" and
    .active == "releases/0.11.2-candidate"' \
    "$v0111_observer_probe/observed.json" >/dev/null \
  || fail 'v0.11.1 post-CAS observer did not interrupt its synthetic link activation'
v0111_unisolated_rc=0
python3 "$v0111_observer" \
  --runtime-root "$v0111_observer_probe/runtime" \
  --journal "$v0111_observer_probe/journal.json" \
  --source-transaction source --candidate-target 0.11.2-candidate \
  --marker "$v0111_observer_probe/unisolated.json" --timeout 1 -- true \
  >/dev/null 2>"$v0111_observer_probe/unisolated.stderr" \
  || v0111_unisolated_rc=$?
[ "$v0111_unisolated_rc" = 2 ] \
  && grep -Fq 'observer must be an isolated process-group leader' \
    "$v0111_observer_probe/unisolated.stderr" \
  || fail 'v0.11.1 post-CAS observer accepts an unisolated process group'
v0111_timeout_probe="$TMP/v0111-post-cas-timeout"
mkdir -p "$v0111_timeout_probe/runtime/releases/source"
ln -s releases/source "$v0111_timeout_probe/runtime/current"
printf '{"transaction":"source","checkpoint":"reconciling"}\n' \
  > "$v0111_timeout_probe/journal.json"
v0111_timeout_rc=0
setsid python3 "$v0111_observer" \
  --runtime-root "$v0111_timeout_probe/runtime" \
  --journal "$v0111_timeout_probe/journal.json" \
  --source-transaction source --candidate-target 0.11.2-candidate \
  --marker "$v0111_timeout_probe/observed.json" --timeout 0.2 -- \
  sh -c '
    while :; do
      printf "mutation\n" >> "$1/mutations"
      sleep 0.02
    done
  ' _ "$v0111_timeout_probe" \
  >/dev/null 2>"$v0111_timeout_probe/stderr" &
v0111_timeout_pid=$!
if wait "$v0111_timeout_pid"; then
  v0111_timeout_rc=0
else
  v0111_timeout_rc=$?
fi
for _ in $(seq 1 100); do
  kill -0 -- "-$v0111_timeout_pid" 2>/dev/null || break
  sleep 0.05
done
[ "$v0111_timeout_rc" = 137 ] \
  && ! kill -0 -- "-$v0111_timeout_pid" 2>/dev/null \
  && [ ! -e "$v0111_timeout_probe/observed.json" ] \
  && [ ! -L "$v0111_timeout_probe/observed.json" ] \
  || fail 'v0.11.1 post-CAS observer timeout did not fence its process group'
v0111_timeout_before="$(sha256sum "$v0111_timeout_probe/mutations")"
sleep 0.2
v0111_timeout_after="$(sha256sum "$v0111_timeout_probe/mutations")"
[ "$v0111_timeout_after" = "$v0111_timeout_before" ] \
  || fail 'v0.11.1 post-CAS observer timeout left a mutating descendant alive'
v0111_exception_probe="$TMP/v0111-post-cas-exception"
mkdir -p "$v0111_exception_probe/runtime/releases/source" \
  "$v0111_exception_probe/runtime/releases/0.11.2-candidate"
ln -s releases/source "$v0111_exception_probe/runtime/current"
printf '{"transaction":"source","checkpoint":"reconciling"}\n' \
  > "$v0111_exception_probe/journal.json"
v0111_exception_rc=0
setsid python3 "$v0111_observer" \
  --runtime-root "$v0111_exception_probe/runtime" \
  --journal "$v0111_exception_probe/journal.json" \
  --source-transaction source --candidate-target 0.11.2-candidate \
  --marker "$v0111_exception_probe/observed.json" --timeout 5 -- \
  sh -c '
    : > "$1/mutations"
    sleep 0.05
    printf "{\"transaction\":\"candidate\",\"checkpoint\":\"activation-intent\",\"goal\":null,\"releases\":{\"target\":\"0.11.2-candidate\"}}\\n" \
      > "$1/journal.next"
    mv "$1/journal.next" "$1/journal.json"
    ln -s releases/0.11.2-candidate "$1/runtime/current.next"
    mv -T "$1/runtime/current.next" "$1/runtime/current"
    while :; do
      printf "mutation\n" >> "$1/mutations"
      sleep 0.02
    done
  ' _ "$v0111_exception_probe" \
  >/dev/null 2>"$v0111_exception_probe/stderr" &
v0111_exception_pid=$!
if wait "$v0111_exception_pid"; then
  v0111_exception_rc=0
else
  v0111_exception_rc=$?
fi
for _ in $(seq 1 100); do
  kill -0 -- "-$v0111_exception_pid" 2>/dev/null || break
  sleep 0.05
done
if [ "$v0111_exception_rc" != 137 ] \
  || kill -0 -- "-$v0111_exception_pid" 2>/dev/null \
  || [ -e "$v0111_exception_probe/observed.json" ] \
  || [ -L "$v0111_exception_probe/observed.json" ]; then
  kill -KILL -- "-$v0111_exception_pid" 2>/dev/null || true
  fail 'v0.11.1 post-CAS observer exception did not fence its process group'
fi
v0111_exception_before="$(sha256sum "$v0111_exception_probe/mutations")"
sleep 0.2
v0111_exception_after="$(sha256sum "$v0111_exception_probe/mutations")"
[ "$v0111_exception_after" = "$v0111_exception_before" ] \
  || fail 'v0.11.1 post-CAS observer exception left a mutating descendant alive'
v0111_queued_probe="$TMP/v0111-post-cas-queued-exit"
mkdir -p "$v0111_queued_probe/runtime/releases/source" \
  "$v0111_queued_probe/runtime/releases/0.11.2-candidate"
ln -s releases/source "$v0111_queued_probe/runtime/current"
printf '{"transaction":"source","checkpoint":"reconciling"}\n' \
  > "$v0111_queued_probe/journal.json"
v0111_queued_rc=0
setsid python3 "$v0111_observer" \
  --runtime-root "$v0111_queued_probe/runtime" \
  --journal "$v0111_queued_probe/journal.json" \
  --source-transaction source --candidate-target 0.11.2-candidate \
  --marker "$v0111_queued_probe/observed.json" --timeout 5 -- \
  sh -c 'printf "ready\n" > "$1/ready"; sleep 0.5; exit 23' \
  _ "$v0111_queued_probe" \
  >/dev/null 2>"$v0111_queued_probe/stderr" &
v0111_queued_pid=$!
for _ in $(seq 1 100); do
  [ -s "$v0111_queued_probe/ready" ] && break
  sleep 0.01
done
[ -s "$v0111_queued_probe/ready" ] \
  || fail 'v0.11.1 queued-event probe child did not start'
kill -STOP "$v0111_queued_pid"
sleep 0.7
printf '{"transaction":"candidate","checkpoint":"activation-intent","goal":{"target":"0.11.2-candidate","direction":"activate-target"},"releases":{"from":"source","target":"0.11.2-candidate"}}\n' \
  > "$v0111_queued_probe/journal.next"
mv "$v0111_queued_probe/journal.next" "$v0111_queued_probe/journal.json"
ln -s releases/0.11.2-candidate "$v0111_queued_probe/runtime/current.next"
mv -T "$v0111_queued_probe/runtime/current.next" \
  "$v0111_queued_probe/runtime/current"
kill -CONT "$v0111_queued_pid"
if wait "$v0111_queued_pid"; then
  v0111_queued_rc=0
else
  v0111_queued_rc=$?
fi
[ "$v0111_queued_rc" = 137 ] \
  && [ ! -e "$v0111_queued_probe/observed.json" ] \
  && [ ! -L "$v0111_queued_probe/observed.json" ] \
  && grep -Fq 'transition exited with status 23 before exact activation' \
    "$v0111_queued_probe/stderr" \
  || fail 'v0.11.1 observer accepted a queued activation after transition exit'
grep -Fq 'POST_CAS_TRANSITION_PID=' "$v0111_recovery" \
  && grep -Fq 'stop_post_cas_transition' "$v0111_recovery" \
  && grep -Fq 'kill -TERM -- "-$POST_CAS_TRANSITION_PID"' "$v0111_recovery" \
  && grep -Fq 'wait "$POST_CAS_TRANSITION_PID"' "$v0111_recovery" \
  || fail 'v0.11.1 cleanup does not fence an interrupted post-CAS process group'
v0111_cleanup_probe="$TMP/v0111-cleanup-probe"
mkdir -p "$v0111_cleanup_probe"
v0111_cleanup_rc=0
bash -c '
  set -euo pipefail
  source "$1"
  probe=$2
  CLEANUP_ARMED=0
  trap cleanup EXIT
  trap "handle_signal 130" INT
  trap "handle_signal 143" TERM
  setsid sh -c '\''
    while :; do
      for resource in project unit reconciler fixture; do
        printf "mutation\n" >> "$1/$resource"
      done
      sleep 0.02
    done
  '\'' _ "$probe" &
  POST_CAS_TRANSITION_PID=$!
  printf "%s\n" "$POST_CAS_TRANSITION_PID" > "$probe/pid"
  for _ in $(seq 1 100); do
    [ -s "$probe/fixture" ] && break
    sleep 0.01
  done
  [ -s "$probe/fixture" ]
  kill -TERM "$$"
' _ "$v0111_recovery" "$v0111_cleanup_probe" || v0111_cleanup_rc=$?
[ "$v0111_cleanup_rc" = 143 ] \
  || fail 'v0.11.1 cleanup swallowed TERM instead of reporting cancellation'
v0111_cleanup_pid="$(cat "$v0111_cleanup_probe/pid")"
! kill -0 -- "-$v0111_cleanup_pid" 2>/dev/null \
  || fail 'v0.11.1 cleanup left its post-CAS process group alive'
v0111_cleanup_before="$(sha256sum \
  "$v0111_cleanup_probe/project" "$v0111_cleanup_probe/unit" \
  "$v0111_cleanup_probe/reconciler" "$v0111_cleanup_probe/fixture")"
sleep 0.2
v0111_cleanup_after="$(sha256sum \
  "$v0111_cleanup_probe/project" "$v0111_cleanup_probe/unit" \
  "$v0111_cleanup_probe/reconciler" "$v0111_cleanup_probe/fixture")"
[ "$v0111_cleanup_after" = "$v0111_cleanup_before" ] \
  || fail 'v0.11.1 post-CAS process mutated resources after cleanup'
v0111_normal_probe="$TMP/v0111-normal-stop-probe"
mkdir -p "$v0111_normal_probe"
bash -c '
  set -euo pipefail
  source "$1"
  probe=$2
  CLEANUP_ARMED=0
  trap cleanup EXIT
  setsid sh -c '\''
    trap "" TERM
    while :; do
      printf "mutation\n" >> "$1/fixture"
      sleep 0.02
    done
  '\'' _ "$probe" &
  POST_CAS_TRANSITION_PID=$!
  printf "%s\n" "$POST_CAS_TRANSITION_PID" > "$probe/pid"
  for _ in $(seq 1 100); do
    [ -s "$probe/fixture" ] && break
    sleep 0.01
  done
  [ -s "$probe/fixture" ]
  stop_post_cas_transition KILL
  [ -z "$POST_CAS_TRANSITION_PID" ]
' _ "$v0111_recovery" "$v0111_normal_probe"
v0111_normal_pid="$(cat "$v0111_normal_probe/pid")"
! kill -0 -- "-$v0111_normal_pid" 2>/dev/null \
  || fail 'normal post-CAS interruption left its process group alive'
v0111_normal_before="$(sha256sum "$v0111_normal_probe/fixture")"
sleep 0.2
v0111_normal_after="$(sha256sum "$v0111_normal_probe/fixture")"
[ "$v0111_normal_after" = "$v0111_normal_before" ] \
  || fail 'normal post-CAS process mutated fixture state after being cleared'
v0111_reaped_probe="$TMP/v0111-reaped-leader-probe"
mkdir -p "$v0111_reaped_probe"
bash -c '
  set -euo pipefail
  source "$1"
  probe=$2
  CLEANUP_ARMED=0
  trap cleanup EXIT
  setsid sh -c '\''
    while :; do
      printf "mutation\n" >> "$1/fixture"
      sleep 0.02
    done &
    sleep 0.05
    exit 23
  '\'' _ "$probe" &
  POST_CAS_TRANSITION_PID=$!
  observed_rc=0
  wait "$POST_CAS_TRANSITION_PID" || observed_rc=$?
  fence_reaped_post_cas_transition "$observed_rc"
  [ "$POST_CAS_TRANSITION_STATUS" = 23 ]
  [ -z "$POST_CAS_TRANSITION_PID" ]
' _ "$v0111_recovery" "$v0111_reaped_probe"
v0111_reaped_before="$(sha256sum "$v0111_reaped_probe/fixture")"
sleep 0.2
v0111_reaped_after="$(sha256sum "$v0111_reaped_probe/fixture")"
[ "$v0111_reaped_after" = "$v0111_reaped_before" ] \
  || fail 'reaped post-CAS observer left its mutating process group alive'
grep -Fq 'STATE_ROOT="/var/tmp/subyard-p0-v0111-$TOKEN"' "$v0111_recovery" \
  && grep -Fq -- '-c features.images=false -c user.subyard.p0="$FIXTURE_MARKER"' "$v0111_recovery" \
  && grep -Fq 'assert_state_root' "$v0111_recovery" \
  && grep -Fq 'assert_fixture_root' "$v0111_recovery" \
  && grep -Fq 'assert_project_marker' "$v0111_recovery" \
  || fail 'v0.11.1 recovery fixture mutations are not marker-bounded'
grep -Fq 'POWER_UNIT="/etc/systemd/system/subyard-p0-v0111-$FIXTURE-$TOKEN.service"' \
    "$v0111_recovery" \
  && grep -Fq 'cleanup_power_runtime' "$v0111_recovery" \
  && grep -Fq 'systemctl disable --now "$POWER_UNIT_NAME"' "$v0111_recovery" \
  && ! grep -Fq 'SUBYARD_POWER_UNIT_PATH=/etc/systemd/system/subyard-power-reconcile.service' \
  "$v0111_recovery" \
  || fail 'v0.11.1 fixture power runtime is not unique, systemd-visible, and cleanup-owned'
grep -Fq 'cleanup_test_vms_sink' "$v0111_recovery" \
  && grep -Fq 'systemctl disable --now "$TEST_VMS_SINK_TIMER_NAME"' "$v0111_recovery" \
  && grep -Fq 'sudo -n find "$FIXTURE_ROOT" -depth -delete' "$v0111_recovery" \
  || fail 'v0.11.1 fixture does not stop its unique sink before privileged marked-root cleanup'
grep -Fq 'capture_recovery_plan' "$v0111_recovery" \
  && grep -Fq '.authorizationPlan == $plan' "$v0111_recovery" \
  && grep -Fq '.outcome.status == "ready"' "$v0111_recovery" \
  && grep -Fq '.assessment.changed == false' "$v0111_recovery" \
  && ! grep -Fq 'config apply --check' "$v0111_recovery" \
  && ! grep -Fq 'grep -Fc -- '\''--yes'\''' "$v0111_recovery" \
  || fail 'v0.11.1 fixture lacks structured plan, terminal, or fixed-point evidence'
v0111_host_free="$ROOT/internal/adapters/releaseruntime/runtime_v0111_recovery_test.go"
grep -Fq 'request.Yard != os.Getenv("SUBYARD_TEST_V0111_YARD")' "$v0111_host_free" \
  && grep -Fq 'fixture.configHome, "recovery-yard", nil' "$v0111_host_free" \
  || fail 'host-free v0.11.1 recovery does not prove the named-yard process request'
ensure_identity
lease_blob="$(awk '{print $2}' "$IDENTITY.pub")"
lease_response="$(printf '{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-001","resource_generation":5,"lease_id":"aabb","capability":"ccdd","lease_epoch":3,"data_user":"subyard-e2e-slot-1","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.1.11","host_key_type":"ssh-ed25519","host_key_blob":"%s"},{"selector":2,"name":"e2e-vm-2","address":"10.42.1.12","host_key_type":"ssh-ed25519","host_key_blob":"%s"}]}}' "$lease_blob" "$lease_blob")"
if (parse_lease_grant "$lease_response") >/dev/null 2>&1; then
  fail "contextless lease grant was accepted"
fi
if (parse_lease_grant '{"status":"ok","grant":{"capability":"secret"}}') >/dev/null 2>&1; then
  fail "incomplete lease grant was accepted"
fi
LEASE_YARD=default
LEASE_PROJECT=Subyard/Attribution
LEASE_RUN='run-a'
LEASE_PURPOSE=contract-tests
structured_response="$(printf '{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-002","resource_generation":5,"lease_id":"eeff","capability":"1122","lease_epoch":4,"context":{"schema_version":2,"yard":"default","project":"Subyard/Attribution","run":"run-a","purpose":"contract-tests"},"data_user":"subyard-e2e-slot-2","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.2.11","host_key_type":"ssh-ed25519","host_key_blob":"%s"},{"selector":2,"name":"e2e-vm-2","address":"10.42.2.12","host_key_type":"ssh-ed25519","host_key_blob":"%s"}]}}' "$lease_blob" "$lease_blob")"
parse_lease_grant "$structured_response" \
  || fail "structured lease grant was rejected"
[ "$LEASE_SLOT" = slot-002 ] && [ "$DATA_USER" = subyard-e2e-slot-2 ] \
  && [ "$LEASE_GENERATION" = 5 ] \
  && [ "${VM_IP[1]}" = 10.42.2.11 ] && [ "${VM_IP[2]}" = 10.42.2.12 ] \
  || fail "lease grant did not materialize exact attributed transport state"
legacy_context="$(jq -c '.grant.context = {schema_version:1, project:"Subyard/Attribution", checkout:"checkout-a", run:"run-a", purpose:"contract-tests"}' <<<"$structured_response")"
if (parse_lease_grant "$legacy_context") >/dev/null 2>&1; then
  fail "legacy lease attribution was accepted"
fi
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
[ "$(grep -c '^    ConnectTimeout 10$' "$CLIENT_CONFIG")" -eq 2 ] \
  && [ "$(grep -c '^    ConnectionAttempts 1$' "$CLIENT_CONFIG")" -eq 2 ] \
  && [ "$(grep -c '^    ServerAliveInterval 15$' "$CLIENT_CONFIG")" -eq 2 ] \
  && [ "$(grep -c '^    ServerAliveCountMax 3$' "$CLIENT_CONFIG")" -eq 2 ] \
  || fail "generated VM transport does not bound an unreachable established session"
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

# Run end-to-end runner fixtures from the synthetic managed workspace too. Using the
# caller checkout here would make these tests pass only when that checkout itself
# happens to live below /srv/workspaces.
RUNNER_UNDER_TEST="$fixture/dev/agent-e2e.sh"
mkdir -p "$fixture/dev" "$fixture/scripts/lib"
cp "$ROOT/dev/agent-e2e.sh" "$RUNNER_UNDER_TEST"
cp "$ROOT/scripts/lib/runtime.sh" "$fixture/scripts/lib/runtime.sh"

new_runner_fixture() {
  local name="$1" key
  RUNNER_FIXTURE="$TMP/runner-$name"
  mkdir -p "$RUNNER_FIXTURE/bin" "$RUNNER_FIXTURE/client/yards/test-yard" \
    "$RUNNER_FIXTURE/routes/test-yard/current"
  ssh-keygen -q -t ed25519 -N '' -f "$RUNNER_FIXTURE/client/id_ed25519"
  ssh-keygen -q -t ed25519 -N '' -f "$RUNNER_FIXTURE/bastion-key"
  key="$(normalized_public_key_file "$RUNNER_FIXTURE/bastion-key.pub")"
  cat > "$RUNNER_FIXTURE/routes/test-yard/current/route.tsv" <<'EOF'
subyard-e2e-route-v1
hostname	127.0.0.1
port	22
host_key_alias	subyard-e2e-bastion
EOF
  printf 'subyard-e2e-bastion %s\n' "$key" \
    > "$RUNNER_FIXTURE/routes/test-yard/current/known_hosts"
  : > "$RUNNER_FIXTURE/facade.log"
  cat > "$RUNNER_FIXTURE/bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
command="${!#}"
busy='{"schema_version":1,"status":"error","code":"busy","state":"held","reason":"busy","owner":{"display_label":"Subyard-2#run-a","yard":"test-yard","project":"Subyard-2","run":"run-a","purpose":"contract-tests","acquired_at":"2026-08-24T12:00:00Z","expires_at":"2026-08-24T12:10:00Z"},"message":"requested slot is unavailable"}'
busy_missing_owner='{"schema_version":1,"status":"error","code":"busy","state":"held","reason":"busy","message":"missing owner"}'
busy_extra_owner='{"schema_version":1,"status":"error","code":"busy","state":"held","reason":"busy","owner":{"display_label":"Subyard-2#run-a","yard":"test-yard","project":"Subyard-2","run":"run-a","purpose":"contract-tests","acquired_at":"2026-08-24T12:00:00Z","expires_at":"2026-08-24T12:10:00Z","client_id":"secret"},"message":"extra owner field"}'
busy_extra_top_level='{"schema_version":1,"status":"error","code":"busy","state":"held","reason":"busy","owner":{"display_label":"Subyard-2#run-a","yard":"test-yard","project":"Subyard-2","run":"run-a","purpose":"contract-tests","acquired_at":"2026-08-24T12:00:00Z","expires_at":"2026-08-24T12:10:00Z"},"message":"requested slot is unavailable","capability":"secret"}'
busy_bad_owner_time='{"schema_version":1,"status":"error","code":"busy","state":"held","reason":"busy","owner":{"display_label":"Subyard-2#run-a","yard":"test-yard","project":"Subyard-2","run":"run-a","purpose":"contract-tests","acquired_at":"later","expires_at":"earlier"},"message":"bad owner time"}'
busy_nonheld_owner='{"schema_version":1,"status":"error","code":"busy","state":"provisioning","reason":"provisioning","owner":{"display_label":"Subyard-2#run-a","yard":"test-yard","project":"Subyard-2","run":"run-a","purpose":"contract-tests","acquired_at":"2026-08-24T12:00:00Z","expires_at":"2026-08-24T12:10:00Z"},"message":"unexpected owner"}'
busy_mismatch='{"schema_version":1,"status":"error","code":"busy","state":"held","reason":"provisioning","message":"untrusted mismatch"}'
busy_unknown_schema='{"schema_version":2,"status":"error","code":"busy","state":"held","reason":"busy","message":"untrusted schema"}'
invalid='{"schema_version":1,"status":"error","code":"invalid_request","reason":"invalid_slot","message":"invalid slot_id"}'
status='{"schema_version":1,"status":"ok","capabilities":["attribution-v2"],"pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-002","resource_generation":1,"lease_epoch":1,"state":"held"}]}}'
status_without_v2='{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-002","resource_generation":1,"lease_epoch":1,"state":"available"}]}}'
grant='{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-002","resource_generation":1,"lease_id":"aabbccdd","lease_epoch":2,"capability":"eeff0011","data_user":"subyard-e2e-slot-2","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.2.11","host_key_type":"ssh-ed25519","host_key_blob":"YWJjZA=="},{"selector":2,"name":"e2e-vm-2","address":"10.42.2.12","host_key_type":"ssh-ed25519","host_key_blob":"YWJjZA=="}]}}'
wrong_grant='{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-001","resource_generation":1,"lease_id":"badc0ffe","lease_epoch":3,"capability":"facefeed","data_user":"subyard-e2e-slot-1","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.1.11","host_key_type":"ssh-ed25519","host_key_blob":"YWJjZA=="},{"selector":2,"name":"e2e-vm-2","address":"10.42.1.12","host_key_type":"ssh-ed25519","host_key_blob":"YWJjZA=="}]}}'
case "$command" in
  status)
    printf '%s\n' "$command" >> "$FAKE_FACADE_LOG"
    if [ "$FAKE_SCENARIO" = status-without-v2 ]; then
      printf '%s\n' "$status_without_v2"
    else
      printf '%s\n' "$status"
    fi
    ;;
  acquire-v2\ *|acquire\ *)
    printf '%s\n' "$command" >> "$FAKE_FACADE_LOG"
    read -r _ _ _ request_yard request_project request_run request_purpose _ _ _ <<<"$command"
    grant="$(jq -c --arg yard "$request_yard" --arg project "$request_project" \
      --arg run "$request_run" --arg purpose "$request_purpose" \
      '.grant.context = {schema_version:2, yard:$yard, project:$project, run:$run, purpose:$purpose}' \
      <<<"$grant")"
    wrong_grant="$(jq -c --arg yard "$request_yard" --arg project "$request_project" \
      --arg run "$request_run" --arg purpose "$request_purpose" \
      '.grant.context = {schema_version:2, yard:$yard, project:$project, run:$run, purpose:$purpose}' \
      <<<"$wrong_grant")"
    count=0
    [ ! -r "$FAKE_ACQUIRE_COUNT" ] || count="$(cat "$FAKE_ACQUIRE_COUNT")"
    count=$((count + 1))
    printf '%s\n' "$count" > "$FAKE_ACQUIRE_COUNT"
    case "$FAKE_SCENARIO" in
      invalid) printf '%s\n' "$invalid" ;;
      busy-missing-owner) printf '%s\n' "$busy_missing_owner" ;;
      busy-extra-owner) printf '%s\n' "$busy_extra_owner" ;;
      busy-extra-top-level) printf '%s\n' "$busy_extra_top_level" ;;
      busy-bad-owner-time) printf '%s\n' "$busy_bad_owner_time" ;;
      busy-nonheld-owner) printf '%s\n' "$busy_nonheld_owner" ;;
      busy-mismatch) printf '%s\n' "$busy_mismatch" ;;
      busy-unknown-schema) printf '%s\n' "$busy_unknown_schema" ;;
      late-grant) sleep 2; printf '%s\n' "$grant" ;;
      outcome-unknown) exit 255 ;;
      wrong-grant) printf '%s\n' "$wrong_grant" ;;
      wait-success) [ "$count" -eq 1 ] && printf '%s\n' "$busy" || printf '%s\n' "$grant" ;;
      *) printf '%s\n' "$busy" ;;
    esac
    ;;
  release\ *)
    printf '%s\n' "$command" >> "$FAKE_FACADE_LOG"
    printf '%s\n' '{"schema_version":1,"status":"ok","message":"released"}'
    ;;
  *) printf 'other %s\n' "$command" >> "$FAKE_FACADE_LOG"; exit 0 ;;
esac
EOF
  chmod +x "$RUNNER_FIXTURE/bin/ssh"
}

run_runner_fixture() {
  local scenario="$1"
  shift
  PATH="$RUNNER_FIXTURE/bin:$PATH" \
    SUBYARD_E2E_TEST_MODE=1 \
    SUBYARD_E2E_STATE_DIR="$RUNNER_FIXTURE/client" \
    SUBYARD_E2E_ROUTE_REGISTRY="$RUNNER_FIXTURE/routes" \
    FAKE_FACADE_LOG="$RUNNER_FIXTURE/facade.log" \
    FAKE_ACQUIRE_COUNT="$RUNNER_FIXTURE/acquire-count" \
    FAKE_SCENARIO="$scenario" \
    "$RUNNER_UNDER_TEST" "$@"
}

# The lease invariant must survive sourcing: no key creation or facade probe without an exact slot.
new_runner_fixture source-invariant
set +e
source_invariant_output="$(
  PATH="$RUNNER_FIXTURE/bin:$PATH" \
    SUBYARD_E2E_TEST_MODE=0 \
    SUBYARD_E2E_STATE_DIR="$RUNNER_FIXTURE/client" \
    SUBYARD_E2E_ROUTE_REGISTRY="$RUNNER_FIXTURE/routes" \
    FAKE_FACADE_LOG="$RUNNER_FIXTURE/facade.log" \
    FAKE_ACQUIRE_COUNT="$RUNNER_FIXTURE/acquire-count" \
    FAKE_SCENARIO=busy \
    bash -c '
      set -euo pipefail
      . "$1/dev/agent-e2e.sh"
      LOCAL_TEMP="$2/local"
      mkdir -p "$LOCAL_TEMP"
      acquire_lease
    ' _ "$fixture" "$RUNNER_FIXTURE" 2>&1
)"
source_invariant_rc=$?
set -e
[ "$source_invariant_rc" = 2 ] \
  && grep -Fq 'an exact --slot is required' <<<"$source_invariant_output" \
  && [ ! -e "$RUNNER_FIXTURE/local/lease_id_ed25519" ] \
  && [ ! -s "$RUNNER_FIXTURE/facade.log" ] \
  || fail "sourced acquire_lease bypassed the exact-slot invariant: $source_invariant_output"

# Every lease-taking mode needs an exact slot before it contacts the facade.
for runner_mode in \
  '--ssh 1 -- true' \
  '--ssh-stdin 1 -- true' \
  '--verify-boundary' \
  '-- true'; do
  new_runner_fixture "missing-slot-${runner_mode%% *}"
  set +e
  missing_slot_output="$(run_runner_fixture busy $runner_mode 2>&1)"
  missing_slot_rc=$?
  set -e
  [ "$missing_slot_rc" = 2 ] \
    && grep -Fq 'an exact --slot is required' <<<"$missing_slot_output" \
    && [ ! -s "$RUNNER_FIXTURE/facade.log" ] \
    || fail "runner accepted a slotless lease mode ($runner_mode): $missing_slot_output"
done

# Informational modes remain slotless and status never issues an acquire request.
new_runner_fixture slotless-info
run_runner_fixture busy --help >/dev/null
run_runner_fixture busy --prepare >/dev/null
status_json="$(run_runner_fixture busy --status --json)"
grep -Fq '"status":"ok"' <<<"$status_json" \
  && [ "$(cat "$RUNNER_FIXTURE/facade.log")" = status ] \
  || fail "slotless informational mode acquired a lease or failed"

# Exact unavailable and invalid responses are reported immediately with their redacted contract fields.
new_runner_fixture attribution-v2-required
set +e
missing_capability_output="$(
  run_runner_fixture status-without-v2 --slot 2 --ssh 1 -- true 2>&1
)"
missing_capability_rc=$?
set -e
[ "$missing_capability_rc" = 2 ] \
  && grep -Fq 'broker does not support required attribution-v2 acquire' \
    <<<"$missing_capability_output" \
  && [ "$(grep -c '^status$' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  && ! grep -q '^acquire' "$RUNNER_FIXTURE/facade.log" \
  || fail "runner downgraded to legacy acquire: $missing_capability_output"

new_runner_fixture exact-busy
set +e
busy_output="$(run_runner_fixture busy --slot 2 --ssh 1 -- true 2>&1)"
busy_rc=$?
set -e
[ "$busy_rc" = 2 ] \
  && grep -Fq 'state=held reason=busy' <<<"$busy_output" \
  && grep -Fq 'owner=test-yard/Subyard-2 run=run-a purpose=contract-tests' <<<"$busy_output" \
  && ! grep -Fq 'client_id' <<<"$busy_output" \
  && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  && grep -Eq '^acquire.* slot-002$' "$RUNNER_FIXTURE/facade.log" \
  || fail "exact busy response was not immediate, redacted, and slot-pinned: $busy_output"

new_runner_fixture exact-invalid
set +e
invalid_output="$(run_runner_fixture invalid --slot 2 --ssh 1 -- true 2>&1)"
invalid_rc=$?
set -e
[ "$invalid_rc" = 2 ] \
  && grep -Fq 'invalid_request: invalid_slot' <<<"$invalid_output" \
  && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  && grep -Eq '^acquire.* slot-002$' "$RUNNER_FIXTURE/facade.log" \
  || fail "invalid exact slot response retried or lost its typed reason: $invalid_output"

# A mismatched successful grant is released with its returned credentials before guest access.
new_runner_fixture wrong-grant
set +e
wrong_grant_output="$(run_runner_fixture wrong-grant --slot 2 --ssh 1 -- true 2>&1)"
wrong_grant_rc=$?
set -e
[ "$wrong_grant_rc" = 2 ] \
  && grep -Fq 'broker returned a slot other than the exact requested slot' <<<"$wrong_grant_output" \
  && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  && [ "$(grep -c '^release slot-001 badc0ffe 3 facefeed$' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  && ! grep -q '^other ' "$RUNNER_FIXTURE/facade.log" \
  || fail "wrong-slot grant was not released before guest access: $wrong_grant_output"

# Bounded waits retry the same slot only after a typed busy response, then use that exact grant.
new_runner_fixture exact-wait-success
set +e
wait_output="$(run_runner_fixture wait-success --slot 2 --wait 10s --ssh 1 -- true 2>&1)"
wait_rc=$?
set -e
[ "$wait_rc" = 0 ] \
  && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" = 2 ] \
  && [ "$(grep '^acquire' "$RUNNER_FIXTURE/facade.log" | grep -vc ' slot-002$')" = 0 ] \
  || fail "bounded exact wait did not retry and acquire only slot-002: $wait_output"

new_runner_fixture exact-wait-timeout
set +e
timeout_output="$(run_runner_fixture busy --slot 2 --wait 1s --ssh 1 -- true 2>&1)"
timeout_rc=$?
set -e
[ "$timeout_rc" = 4 ] \
  && grep -Fq 'state=held reason=busy' <<<"$timeout_output" \
  && grep -Fq 'owner=test-yard/Subyard-2 run=run-a purpose=contract-tests' <<<"$timeout_output" \
  && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" -ge 1 ] \
  && [ "$(grep '^acquire' "$RUNNER_FIXTURE/facade.log" | grep -vc ' slot-002$')" = 0 ] \
  || fail "bounded exact wait did not return busy with its last redacted state: $timeout_output"

# A transport failure may hide a successful allocation, so it must never be retried.
new_runner_fixture exact-outcome-unknown
set +e
unknown_output="$(run_runner_fixture outcome-unknown --slot 2 --wait 6s --ssh 1 -- true 2>&1)"
unknown_rc=$?
set -e
[ "$unknown_rc" = 2 ] \
  && grep -Fq 'lease acquire outcome is unknown; refusing a second allocation' <<<"$unknown_output" \
  && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  && grep -Eq '^acquire.* slot-002$' "$RUNNER_FIXTURE/facade.log" \
  || fail "outcome-unknown acquire was retried or lost exact-slot pinning: $unknown_output"

# Only the exact broker schema and stable state/reason pairs prove that no allocation occurred.
for malformed_busy_scenario in \
  busy-missing-owner busy-extra-owner busy-extra-top-level busy-bad-owner-time busy-nonheld-owner \
  busy-mismatch busy-unknown-schema; do
  new_runner_fixture "exact-$malformed_busy_scenario"
  set +e
  malformed_busy_output="$(
    run_runner_fixture "$malformed_busy_scenario" --slot 2 --wait 1s --ssh 1 -- true 2>&1
  )"
  malformed_busy_rc=$?
  set -e
  [ "$malformed_busy_rc" = 2 ] \
    && grep -Fq 'lease acquire outcome is unknown; refusing a second allocation' \
      <<<"$malformed_busy_output" \
    && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
    || fail "non-contract busy response was retried ($malformed_busy_scenario): $malformed_busy_output"
done

# A grant that arrives after the bounded deadline is released without guest access.
new_runner_fixture exact-late-grant
set +e
late_grant_output="$(run_runner_fixture late-grant --slot 2 --wait 1s --ssh 1 -- true 2>&1)"
late_grant_rc=$?
set -e
[ "$late_grant_rc" = 4 ] \
  && grep -Fq 'deadline elapsed before the lease grant arrived' <<<"$late_grant_output" \
  && [ "$(grep -c '^acquire' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  && [ "$(grep -c '^release' "$RUNNER_FIXTURE/facade.log")" = 1 ] \
  || fail "late exact-slot grant was accepted or not released: $late_grant_output"

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

# Public operational examples and direct script calls must keep the exact-slot boundary visible.
# Read the files themselves so a source archive cannot silently skip this check.
(
  cd "$ROOT"
  find AGENTS.md docs dev -type f \( -name '*.md' -o -name '*.sh' \) -print0 \
    | xargs -0 grep -nHE 'dev/agent-e2e\.sh|dev/e2e/p0-acceptance\.sh|\$RUNNER'
) > "$TMP/public-callers"
[ -s "$TMP/public-callers" ] || fail 'public caller inventory is empty'
slotless_public_callers=''
while IFS=: read -r caller_path caller_line caller_source; do
  trimmed_source="${caller_source#"${caller_source%%[![:space:]]*}"}"
  caller_kind=''
  case "$caller_path" in
    *.sh)
      case "$trimmed_source" in
        dev/agent-e2e.sh\ *) caller_kind=runner ;;
        dev/e2e/p0-acceptance.sh*) caller_kind=p0 ;;
        *'"$RUNNER" '*) caller_kind=runner ;;
      esac
      ;;
    *.md)
      case "$trimmed_source" in
        dev/agent-e2e.sh\ *|*'`dev/agent-e2e.sh '*) caller_kind=runner ;;
        dev/e2e/p0-acceptance.sh*|*'`dev/e2e/p0-acceptance.sh'*) caller_kind=p0 ;;
      esac
      ;;
  esac
  case "$caller_kind" in
    runner)
      case "$caller_source" in
        *--slot*|*--prepare*|*--status*|*--help*) ;;
        *) slotless_public_callers+="${caller_path}:${caller_line}:${caller_source}"$'\n' ;;
      esac
      ;;
    p0)
      case "$caller_source" in
        *--slot*|*--list-lanes*|*--help*) ;;
        *) slotless_public_callers+="${caller_path}:${caller_line}:${caller_source}"$'\n' ;;
      esac
      ;;
  esac
done < "$TMP/public-callers"
[ -z "$slotless_public_callers" ] \
  || fail "public lease-taking caller lacks exact --slot:\n$slotless_public_callers"

P0_REAL_INCUS_WAIT_FUNCTION="$(sed -n '/^wait_ready() {/,/^}/p' "$ROOT/dev/e2e/p0-real-incus.sh")" \
  bash -c '
    set -euo pipefail
    eval "$P0_REAL_INCUS_WAIT_FUNCTION"
    PROJECT=test-project
    die() { return 2; }
    sleep() { SECONDS=$((SECONDS + 180)); }
    real_incus_observe() {
      case "$1" in
        exec) [ "$case_name" != stuck-vm ] && [ "$((SECONDS - started))" -ge 300 ] ;;
        list) printf "RUNNING\n" ;;
        info|console) diagnostics="$diagnostics $1" ;;
        *) return 3 ;;
      esac
    }
    for case_name in slow-vm slow-container stuck-vm; do
      started=$SECONDS
      diagnostics=""
      kind=virtual-machine
      [ "$case_name" != slow-container ] || kind=container
      if wait_ready test-instance "$kind"; then
        [ "$case_name" = slow-vm ]
        [ -z "$diagnostics" ]
      else
        [ "$case_name" != slow-vm ]
        [ "$diagnostics" = " info console" ]
        [ "$((SECONDS - started))" -le 720 ]
      fi
    done
  ' || fail "P0 readiness does not allow slow VM boot or diagnose bounded failures"
grep -Fq 'cleanup delete of %s failed; retrying (%s/3)' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'refusing to delete unmarked instance' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'could not delete marked instance $name after 3 attempts' "$ROOT/dev/e2e/p0-real-incus.sh" \
  || fail "P0 real-Incus cleanup retry is not bounded to marked test instances"
real_incus_project_cleanup_functions="$(
  for function_name in real_incus_observe p0_monotonic_seconds cleanup_real_incus \
    cleanup_sleep active_instance_operation_ids delete_marked_instance cleanup; do
    sed -n "/^${function_name}() {/,/^}/p" "$ROOT/dev/e2e/p0-real-incus.sh"
  done
)"
run_real_incus_project_cleanup_case() {
  REAL_INCUS_PROJECT_CLEANUP_CASE="$1" \
  REAL_INCUS_PROJECT_CLEANUP_LOG="$2" \
  REAL_INCUS_PROJECT_CLEANUP_FUNCTIONS="$real_incus_project_cleanup_functions" bash -c '
    set -euo pipefail
    MARKER=agent-e2e-p0
    PROJECT=subyard-p0-real-incus
    TMP=""
    container_present=1
    vm_present=1
    project_present=1
    die() { printf "%s\\n" "$*" >&2; exit 2; }
    eval "$REAL_INCUS_PROJECT_CLEANUP_FUNCTIONS"
    real_incus() {
      printf "%s\\n" "$*" >> "$REAL_INCUS_PROJECT_CLEANUP_LOG"
      case "$1 $2" in
        "project list")
          [ "$REAL_INCUS_PROJECT_CLEANUP_CASE" != inventory-error ] || return 17
          [ "$project_present" = 0 ] || printf "%s\\n" "$PROJECT"
          ;;
        "project get")
          [ "$REAL_INCUS_PROJECT_CLEANUP_CASE" != foreign-project ] \
            && printf "%s\\n" "$MARKER" || printf "%s\\n" foreign
          ;;
        "list p0-container")
          if [ "$REAL_INCUS_PROJECT_CLEANUP_CASE" != foreign-instance ] \
            && [ "$container_present" = 1 ]; then
            printf "%s\\n" p0-container
          fi
          ;;
        "list p0-vm")
          [ "$vm_present" != 1 ] || printf "%s\\n" p0-vm
          ;;
        "operation list") printf "%s\\n" "[]" ;;
        "config get")
          [ "$REAL_INCUS_PROJECT_CLEANUP_CASE" != foreign-instance-marker ] \
            && printf "%s\\n" "$MARKER" || printf "%s\\n" foreign
          ;;
        "delete p0-container") container_present=0 ;;
        "delete p0-vm") vm_present=0 ;;
        "list --project")
          [ "$container_present" = 0 ] && [ "$vm_present" = 0 ] || printf "%s\\n" unexpected
          ;;
        "project delete") project_present=0 ;;
        "image "*) exit 94 ;;
        *) exit 93 ;;
      esac
    }
    cleanup
    [ "$project_present" = 0 ]
  '
}
real_incus_project_cleanup_log="$TMP/real-incus-project-cleanup.log"
: > "$real_incus_project_cleanup_log"
run_real_incus_project_cleanup_case marked "$real_incus_project_cleanup_log" \
  || fail 'P0 cleanup did not remove the marked real-Incus fixture'
grep -Fqx 'project delete subyard-p0-real-incus' "$real_incus_project_cleanup_log" \
  || fail 'P0 cleanup did not delete its empty marked project'
for real_incus_project_cleanup_case in foreign-project foreign-instance foreign-instance-marker inventory-error; do
  : > "$real_incus_project_cleanup_log"
  set +e
  run_real_incus_project_cleanup_case "$real_incus_project_cleanup_case" \
    "$real_incus_project_cleanup_log" >/dev/null 2>&1
  real_incus_project_cleanup_rc=$?
  set -e
  [ "$real_incus_project_cleanup_rc" = 2 ] \
    && ! grep -Fq 'project delete subyard-p0-real-incus' "$real_incus_project_cleanup_log" \
    && ! grep -Fq 'image ' "$real_incus_project_cleanup_log" \
    || fail "P0 cleanup changed an $real_incus_project_cleanup_case fixture or its cache"
done
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
              "[{\"id\":\"create-p0-vm\",\"status_code\":103,\"resources\":{\"instances\":[\"/1.0/instances/p0-vm?project=subyard-p0-real-incus\"]}}]"
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
(
  source_root="$ROOT"
  bootstrap_fixture="$TMP/incus-bootstrap-isolation"
  mkdir -p "$bootstrap_fixture/repo/tests/helpers" "$bootstrap_fixture/repo/config" \
    "$bootstrap_fixture/repo/scripts" "$bootstrap_fixture/state" "$bootstrap_fixture/platform/incus"
  cp "$ROOT/tests/helpers/test-context.sh" "$bootstrap_fixture/repo/tests/helpers/"
  cp "$ROOT/config/host.env" "$bootstrap_fixture/repo/config/"
  # shellcheck source=dev/e2e/lib-p0-capacity.sh
  . "$ROOT/dev/e2e/lib-p0-capacity.sh"
  P0_CAPACITY_STATE_ROOT="$bootstrap_fixture/state"
  P0_CAPACITY_PLATFORM_ROOT="$bootstrap_fixture/platform"
  P0_CAPACITY_MARKER='bootstrap-fixture'
  printf '%s\n' "$P0_CAPACITY_MARKER" > "$P0_CAPACITY_STATE_ROOT/.subyard-p0-marker"
  printf 'retained\n' > "$P0_CAPACITY_PLATFORM_ROOT/incus/sentinel"
  chmod 0555 "$P0_CAPACITY_PLATFORM_ROOT/incus"
  p0_capacity_prepare_platform_root() { :; }
  export EXPECTED_BOOTSTRAP="$P0_CAPACITY_STATE_ROOT/incus-bootstrap"
  export EXPECTED_STORAGE="$P0_CAPACITY_PLATFORM_ROOT/incus/incus/storage"
  export BOOTSTRAP_CALL_LOG="$bootstrap_fixture/calls" BOOTSTRAP_EXIT
  cat > "$bootstrap_fixture/repo/scripts/01-install-incus.sh" <<'BOOTSTRAP_INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
[ "$SUBYARD_CONFIG_HOME" = "$EXPECTED_BOOTSTRAP/config" ]
[ "$SUBYARD_HOME" = "$EXPECTED_BOOTSTRAP/subyard" ]
[ -d "$SUBYARD_HOME" ] && [ -O "$SUBYARD_HOME" ]
[ "$STORAGE_PATH" = "$EXPECTED_STORAGE" ]
[ "$HOST_BASE" = "$EXPECTED_BOOTSTRAP/host-data" ]
printf 'called\n' >> "$BOOTSTRAP_CALL_LOG"
exit "$BOOTSTRAP_EXIT"
BOOTSTRAP_INSTALLER
  ROOT="$bootstrap_fixture/repo"
  for bootstrap_caller in p0-guest.sh p0-source-upgrade.sh; do
    eval "$(sed -n '/^run_incus_installer() {/,/^}/p' "$source_root/dev/e2e/$bootstrap_caller")"
    for BOOTSTRAP_EXIT in 0 27; do
      bootstrap_rc=0
      if [ "$bootstrap_caller" = p0-guest.sh ]; then
        run_incus_installer "$P0_CAPACITY_PLATFORM_ROOT" "$EXPECTED_STORAGE" --yes || bootstrap_rc=$?
      else
        run_incus_installer --yes || bootstrap_rc=$?
      fi
      [ "$bootstrap_rc" = "$BOOTSTRAP_EXIT" ] || fail 'Incus bootstrap lost installer exit status'
      [ ! -e "$EXPECTED_BOOTSTRAP" ] || fail 'Incus bootstrap left ephemeral operator state'
      [ "$(cat "$P0_CAPACITY_PLATFORM_ROOT/incus/sentinel")" = retained ] \
        && [ "$(stat -c '%a' "$P0_CAPACITY_PLATFORM_ROOT/incus")" = 555 ] \
        || fail 'Incus bootstrap changed persistent parent contents or permissions'
    done
  done
  [ "$(wc -l < "$BOOTSTRAP_CALL_LOG")" = 4 ] || fail 'Incus bootstrap skipped the installer'
  chmod 0755 "$P0_CAPACITY_PLATFORM_ROOT/incus"
)
# Exercise the actual standalone dispatch with VM boundaries replaced by a strict
# fixture state machine. An unprepared reboot must fail, even on an empty host.
reboot_lane_dispatch="$(awk '
  /^case "\$P0_LANE" in$/ { block = ""; capture = 1 }
  capture { block = block $0 ORS }
  /^esac$/ { if (capture) { result = block; capture = 0 } }
  END { printf "%s", result }
' "$ROOT/dev/e2e/p0-acceptance.sh")"
reboot_lane_function="$(sed -n '/^reboot_verify_lane() {/,/^}/p' "$ROOT/dev/e2e/p0-acceptance.sh")"
power_systemd_lane_body="$(sed -n '/^power_systemd_lane() {/,/^}/p' "$ROOT/dev/e2e/p0-acceptance.sh")"
for reboot_scenario in reboot-verify:none power-systemd:none reboot-verify:prepare reboot-verify:reboot reboot-verify:resume reboot-verify:finish; do
  reboot_failure="${reboot_scenario#*:}"
  reboot_fixture_log="$TMP/reboot-fixture-$reboot_scenario.log"
  set +e
  REBOOT_LANE_DISPATCH="$reboot_lane_dispatch" REBOOT_LANE_FUNCTION="$reboot_lane_function" \
    POWER_LANE_FUNCTION="$power_systemd_lane_body" REBOOT_FAILURE="$reboot_failure" \
    REBOOT_LANE="${reboot_scenario%%:*}" bash -c '
      set -euo pipefail
      P0_LANE="$REBOOT_LANE"
      TOKEN=123
      POWER_SYSTEMD_STARTED=0
      POWER_SYSTEMD_LANE_VM=1
      platform=0
      fixture=absent
      reboots=0
      run_phase() { shift; "$@"; }
      run_vm() {
        case "$*" in
          "1 capacity-preflight") printf "capacity\n" ;;
          "1 real-incus") platform=1; printf "platform\n" ;;
          *) exit 41 ;;
        esac
      }
      run_power_systemd_vm() {
        case "$*" in
          "1 dev/e2e/power-reconciler-systemd-255.sh") printf "systemd255\n"; return ;;
          "1 dev/e2e/power-reconciler-systemd.sh") printf "systemd\n"; return ;;
        esac
        [ "$1" = 1 ] && [ "$2" = dev/e2e/power-reconciler-upgrade.sh ] && [ "$4" = 123 ] || exit 42
        [ "$platform" = 1 ] && [ "$POWER_SYSTEMD_STARTED" = 1 ] || exit 43
        printf "%s\n" "$3"
        [ "$REBOOT_FAILURE" != "$3" ] || exit 45
        case "$3:$fixture:$reboots" in
          prepare:absent:0) fixture=prepared ;;
          resume:prepared:1) fixture=resumed ;;
          finish:resumed:2) fixture=restored ;;
          *) exit 44 ;;
        esac
      }
      reboot_vm() {
        [ "$1" = 1 ] && [ "$POWER_SYSTEMD_STARTED" = 1 ] || exit 46
        case "$fixture:$reboots" in prepared:0|resumed:1) ;; *) exit 47 ;; esac
        printf "reboot\n"
        [ "$REBOOT_FAILURE" != reboot ] || exit 45
        reboots=$((reboots + 1))
      }
      cleanup_lane() {
        [ "$fixture" = restored ] && [ "$POWER_SYSTEMD_STARTED" = 0 ] || exit 48
        printf "cleanup\n"
      }
      trap '\''printf "exit=%s armed=%s\n" "$?" "$POWER_SYSTEMD_STARTED"'\'' EXIT
      eval "$REBOOT_LANE_FUNCTION"
      eval "$POWER_LANE_FUNCTION"
      eval "$REBOOT_LANE_DISPATCH"
    ' >"$reboot_fixture_log" 2>&1
  reboot_fixture_rc=$?
  set -e
  if [ "$reboot_failure" = none ]; then
    [ "$reboot_fixture_rc" = 0 ] \
      || fail 'standalone reboot lane did not prepare its own power fixture before rebooting'
    reboot_expected=$'capacity\nplatform\n'
    [ "${reboot_scenario%%:*}" != power-systemd ] || reboot_expected+=$'systemd255\nsystemd\n'
    reboot_expected+=$'prepare\nreboot\nresume\nreboot\nfinish\ncleanup\nexit=0 armed=0'
    [ "$(cat "$reboot_fixture_log")" = "$reboot_expected" ] \
      || fail 'standalone reboot lane lost ordered preparation, two reboots, or runtime restoration'
  else
    [ "$reboot_fixture_rc" = 45 ] && tail -n 1 "$reboot_fixture_log" | grep -Fxq 'exit=45 armed=1' \
      || fail "standalone reboot lane concealed $reboot_failure failure or disarmed fixture recovery"
  fi
done

release_smoke_function="$(sed -n '/^release_smoke_lane() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-acceptance.sh")"
for release_smoke_failure in success prepare reboot finish; do
  release_smoke_log="$TMP/release-smoke-$release_smoke_failure.log"
  set +e
  RELEASE_SMOKE_FUNCTION="$release_smoke_function" \
    RELEASE_SMOKE_FAILURE="$release_smoke_failure" bash -c '
      set -euo pipefail
      eval "$RELEASE_SMOKE_FUNCTION"
      TOKEN=123
      POWER_SYSTEMD_STARTED=0
      POWER_SYSTEMD_LANE_VM=0
      run_power_systemd_vm() {
        [ "$1" = 1 ] && [ "$2" = dev/e2e/p0-release-smoke.sh ] || return 44
        printf "%s\n" "$3"
        [ "$RELEASE_SMOKE_FAILURE" != "$3" ] || return 45
      }
      reboot_vm() {
        [ "$1" = 1 ] || return 44
        printf "reboot\n"
        [ "$RELEASE_SMOKE_FAILURE" != reboot ] || return 45
      }
      trap '\''printf "exit=%s armed=%s vm=%s\n" "$?" \
        "$POWER_SYSTEMD_STARTED" "$POWER_SYSTEMD_LANE_VM"'\'' EXIT
      release_smoke_lane
    ' >"$release_smoke_log" 2>&1
  release_smoke_rc=$?
  set -e
  case "$release_smoke_failure" in
    success)
      [ "$release_smoke_rc" = 0 ] \
        && [ "$(cat "$release_smoke_log")" = \
          $'prepare\nreboot\nfinish\nexit=0 armed=0 vm=1' ] \
        || fail 'release smoke lost its ordered VM1 reboot flow or left cleanup armed'
      ;;
    prepare)
      release_smoke_expected=$'prepare\nexit=45 armed=1 vm=1'
      ;;
    reboot)
      release_smoke_expected=$'prepare\nreboot\nexit=45 armed=1 vm=1'
      ;;
    finish)
      release_smoke_expected=$'prepare\nreboot\nfinish\nexit=45 armed=1 vm=1'
      ;;
  esac
  if [ "$release_smoke_failure" != success ]; then
    [ "$release_smoke_rc" = 45 ] \
      && [ "$(cat "$release_smoke_log")" = "$release_smoke_expected" ] \
      || fail "release smoke concealed $release_smoke_failure failure or disarmed cleanup"
  fi
done

# Switching fixture operators must isolate identity and the private working directory.
(
  eval "$(sed -n '/^operator_env() {/,/^}/p' "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
  OPERATOR='fixture-operator'
  OPERATOR_HOME="$TMP/operator-home"
  mkdir -p "$OPERATOR_HOME"
  operator_uid() { printf '2000\n'; }
  sudo() {
    [ "$1" = -n ] && [ "$2" = /usr/sbin/runuser ] \
      && [ "$3" = -u ] && [ "$4" = "$OPERATOR" ] && [ "$5" = -- ] || return 1
    shift 5
    SUDO_USER=caller SUDO_UID=1000 SUDO_GID=1000 SUDO_COMMAND=runuser "$@"
  }
  operator_env bash -eu -c '
    [ "$USER" = fixture-operator ] && [ "$LOGNAME" = "$USER" ]
    [ -z "${SUDO_USER+x}${SUDO_UID+x}${SUDO_GID+x}${SUDO_COMMAND+x}" ]
    [ "$XDG_RUNTIME_DIR" = /run/user/2000 ]
    [ "$PWD" = "$HOME" ]
  '
) || fail 'fixture operator inherited the caller sudo identity or working directory'

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
      exercise_activation_only_repair() { log exercise_activation_only_repair; }
      record_reboot_baseline() { log record_reboot_baseline; }
      write_fixture_value() { log "write_fixture_value $*"; }
      finish_candidate_flow() { log finish_candidate_flow; }
      assert_state_root() { log assert_state_root; }
      assert_fixture_phase() { log "assert_fixture_phase $*"; }
      assert_post_reboot_candidate() { log assert_post_reboot_candidate; }
      operator_yard() { log "operator_yard $*"; }
      assert_candidate_state() { log assert_candidate_state; }
      trap '\''log "exit preserve=$PRESERVE_FIXTURE cleanup=$CLEANUP_ARMED"'\'' EXIT
      eval "$UPGRADE_DISPATCHER"
    ' >/dev/null 2>&1
  dispatcher_rc=$?
  set -e
  [ "$dispatcher_rc" = 0 ] \
    || fail "power reconciler $dispatcher_mode dispatcher rejected its valid phase"
  case "$dispatcher_mode" in
    prepare)
      dispatcher_expected=$'incus image info subyard-e2e-debian-13-cloud-container --project default\nprepare_candidate\nexercise_activation_only_repair\nrecord_reboot_baseline\nwrite_fixture_value /state/phase candidate-ready\nexit preserve=1 cleanup=0'
      ;;
    resume)
      dispatcher_expected=$'assert_state_root\nassert_fixture_phase candidate-ready\nassert_post_reboot_candidate\noperator_yard init --yes\nassert_candidate_state\nrecord_reboot_baseline\nwrite_fixture_value /state/phase candidate-reconciled\nexit preserve=1 cleanup=1'
      ;;
    finish)
      dispatcher_expected=$'assert_state_root\nassert_fixture_phase candidate-reconciled\nassert_post_reboot_candidate\nfinish_candidate_flow\nexit preserve=0 cleanup=1'
      ;;
  esac
  [ "$(cat "$dispatcher_log")" = "$dispatcher_expected" ] \
    || fail "power reconciler $dispatcher_mode dispatcher lost its ordered incident flow"
done
activation_repair_functions="$(
  sed -n '/^materialize_unit() {/,/^}/p' "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
  sed -n '/^assert_activation_only_journal() {/,/^}/p' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
  sed -n '/^assert_activation_ledger_unchanged() {/,/^}/p' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
  sed -n '/^install_activation_reconcile_fault() {/,/^}/p' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
  sed -n '/^exercise_activation_only_repair() {/,/^}/p' \
    "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
)"
run_activation_repair_contract() (
  set -euo pipefail
  local scenario="$1"
  local fixture="$TMP/power-activation-$scenario"
  STATE_ROOT="$fixture/state"
  ROOT="$fixture/candidate-root"
  OPERATOR_HOME="$fixture/operator"
  RELEASE_ROOT="$fixture/release"
  RECONCILER="$fixture/yard-boot-reconcile"
  UNIT="$fixture/subyard-power-reconcile.service"
  OLD_UNIT_FIXTURE="$fixture/v0.8.service.in"
  ACTIVATION_LEDGER_BASELINE="$STATE_ROOT/activation-ledger.before"
  ACTIVATION_JOURNAL_BASELINE="$STATE_ROOT/activation-journal.before"
  ACTIVATION_FAULT_PROBE="$OPERATOR_HOME/activation-faults"
  ACTIVATION_SYSTEMCTL_WRAPPER="$OPERATOR_HOME/.local/bin/systemctl"
  ACTIVATION_SYSTEMCTL_DELEGATE="$fixture/systemctl-real"
  V2_STATE_ROOT="$OPERATOR_HOME/.config/subyard/release-transition/v2"
  V2_LEDGER="$V2_STATE_ROOT/ledger.json"
  V2_JOURNAL="$V2_STATE_ROOT/journal.json"
  CANDIDATE_RELEASE_TARGET=releases/candidate-aaaaaaaaaaaa
  OLD_RELEASE_TARGET=releases/0.8.0-bbbbbbbbbbbb
  CANDIDATE_VERSION=candidate
  OPERATOR='fixture-operator'
  FAKE_YARD_CALLS="$fixture/yard-calls"
  DELEGATE_LOG="$fixture/systemctl-delegate"
  V1_HISTORY_LOG="$fixture/v1-history"
  CANDIDATE_UNIT="$STATE_ROOT/candidate-activation.service"
  ACTIVATION_SCENARIO="$scenario"
  export ACTIVATION_FAULT_PROBE ACTIVATION_SCENARIO ACTIVATION_SYSTEMCTL_DELEGATE
  export ACTIVATION_SYSTEMCTL_WRAPPER CANDIDATE_UNIT DELEGATE_LOG FAKE_YARD_CALLS
  export ACTIVATION_JOURNAL_BASELINE ACTIVATION_LEDGER_BASELINE CANDIDATE_VERSION
  export OPERATOR RECONCILER UNIT V2_JOURNAL V2_LEDGER

  mkdir -p "$STATE_ROOT" "$ROOT/config/systemd" "$OPERATOR_HOME/.local/bin" \
    "$V2_STATE_ROOT" "$RELEASE_ROOT"
  printf 'ExecStart=@SUBYARD_POWER_RECONCILER@\nGeneration=candidate\n' \
    > "$ROOT/config/systemd/subyard-power-reconcile.service.in"
  printf 'ExecStart=@SUBYARD_POWER_RECONCILER@\nGeneration=v0.8\n' > "$OLD_UNIT_FIXTURE"
  printf '{"ledger":"stable"}\n' > "$V2_LEDGER"
  printf '%s\n' \
    '{"schemaVersion":2,"transaction":"completed-transaction","authorizationDigest":"completed-authorization","goal":{"target":"candidate-aaaaaaaaaaaa","direction":"activate-target"},"releases":{"from":"candidate-aaaaaaaaaaaa","target":"candidate-aaaaaaaaaaaa"},"checkpoint":"complete","steps":[]}' \
    > "$V2_JOURNAL"
  : > "$FAKE_YARD_CALLS"
  : > "$DELEGATE_LOG"
  : > "$V1_HISTORY_LOG"
  printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    'printf "%s\n" "$*" >> "$DELEGATE_LOG"' \
    > "$ACTIVATION_SYSTEMCTL_DELEGATE"
  chmod 0755 "$ACTIVATION_SYSTEMCTL_DELEGATE"
  printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    'calls=$(wc -l < "$FAKE_YARD_CALLS")' \
    'case "$calls" in' \
    '  0)' \
    '    case " $* " in *" --yes "*) exit 66 ;; esac' \
    '    printf "unconfirmed:%s\n" "$*" >> "$FAKE_YARD_CALLS"' \
    '    case "$ACTIVATION_SCENARIO" in' \
    '      unconfirmed-unit-mutation) printf "unauthorized mutation\n" >> "$UNIT" ;;' \
    '      unconfirmed-journal-mutation)' \
    '        jq '\''.checkpoint = "unauthorized-mutation"'\'' "$V2_JOURNAL" > "$V2_JOURNAL.next"' \
    '        mv "$V2_JOURNAL.next" "$V2_JOURNAL"' \
    '        ;;' \
    '      unconfirmed-ledger-mutation) printf "unauthorized mutation\n" >> "$V2_LEDGER" ;;' \
    '    esac' \
    '    printf "confirmation required: interactive terminal required\n" >&2' \
    '    exit 1' \
    '    ;;' \
    '  1)' \
    '    case " $* " in *" --yes "*) ;; *) exit 67 ;; esac' \
    '    printf "authorized:%s\n" "$*" >> "$FAKE_YARD_CALLS"' \
    '    [ "$ACTIVATION_SCENARIO" != unconfirmed-ledger-mutation ] || cp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER"' \
    '    cp "$CANDIDATE_UNIT" "$UNIT"' \
    '    jq '\''.transaction = "repair-transaction" | .authorizationDigest = "repair-authorization" | .checkpoint = "reconciling"'\'' "$V2_JOURNAL" > "$V2_JOURNAL.next"' \
    '    mv "$V2_JOURNAL.next" "$V2_JOURNAL"' \
    '    set +e' \
    '    "$ACTIVATION_SYSTEMCTL_WRAPPER" show subyard-power-reconcile.service --property=LoadState >/dev/null 2>&1' \
    '    fault_rc=$?' \
    '    set -e' \
    '    [ "$fault_rc" = 75 ] || exit 68' \
    '    if [ -f "$ACTIVATION_FAULT_PROBE" ] &&' \
    '      [ "$(wc -l < "$ACTIVATION_FAULT_PROBE")" = 1 ]; then' \
    '      "$ACTIVATION_SYSTEMCTL_WRAPPER" show subyard-power-reconcile.service --property=LoadState >/dev/null' \
    '    fi' \
    '    exit 75' \
    '    ;;' \
    '  2)' \
    '    case " $* " in *" --yes "*) exit 69 ;; esac' \
    '    printf "resume:%s\n" "$*" >> "$FAKE_YARD_CALLS"' \
    '    jq '\''.checkpoint = "complete"'\'' "$V2_JOURNAL" > "$V2_JOURNAL.next"' \
    '    mv "$V2_JOURNAL.next" "$V2_JOURNAL"' \
    '    ;;' \
    '  *) exit 70 ;;' \
    'esac' \
    > "$OPERATOR_HOME/.local/bin/yard"
  chmod 0755 "$OPERATOR_HOME/.local/bin/yard"

  eval "$activation_repair_functions"
  die() { printf 'activation fixture: %s\n' "$*" >&2; exit 2; }
  ok() { :; }
  load_release_targets() { :; }
  operator_env() { "$@"; }
  operator_yard() { operator_env "$OPERATOR_HOME/.local/bin/yard" "$@"; }
  sudo() {
    [ "${1:-}" != -n ] || shift
    if [ "${1:-}" = systemctl ]; then
      [ "${2:-}" = daemon-reload ] || return 71
      return 0
    fi
    if [ "${1:-}" = install ]; then
      shift
      local -a arguments=()
      while [ "$#" -gt 0 ]; do
        case "$1" in
          -o|-g) shift 2 ;;
          *) arguments+=("$1"); shift ;;
        esac
      done
      command install "${arguments[@]}"
      return
    fi
    command "$@"
  }
  assert_published_v1_history_unchanged() { printf 'checked\n' >> "$V1_HISTORY_LOG"; }
  assert_candidate_state() { cmp "$CANDIDATE_UNIT" "$UNIT"; }
  assert_runtime_links() {
    [ "$1" = "$CANDIDATE_RELEASE_TARGET" ] && [ "$2" = "$OLD_RELEASE_TARGET" ]
  }

  install_activation_reconcile_fault
  "$ACTIVATION_SYSTEMCTL_WRAPPER" show subyard-power-reconcile.service \
    --property=LoadState >/dev/null || exit 72
  [ ! -e "$ACTIVATION_FAULT_PROBE" ] || exit 73
  grep -Fxq 'show subyard-power-reconcile.service --property=LoadState' "$DELEGATE_LOG" \
    || exit 74
  : > "$DELEGATE_LOG"
  find "$ACTIVATION_SYSTEMCTL_WRAPPER" -delete

  exercise_activation_only_repair
  [ "$(cat "$FAKE_YARD_CALLS")" = "$(printf '%s\n%s\n%s' \
    'unconfirmed:update --version candidate' \
    'authorized:update --version candidate --yes' \
    'resume:update --version candidate')" ] || exit 75
  [ "$(wc -l < "$ACTIVATION_FAULT_PROBE")" = 1 ] || exit 76
  grep -Fxq 'show subyard-power-reconcile.service --property=LoadState' "$DELEGATE_LOG" \
    || exit 77
  [ "$(wc -l < "$V1_HISTORY_LOG")" = 3 ] || exit 78
  jq -e '.transaction == "repair-transaction" and
    .authorizationDigest == "repair-authorization" and
    .checkpoint == "complete" and (.steps | length) == 0' \
    "$V2_JOURNAL" >/dev/null || exit 79
  cmp "$CANDIDATE_UNIT" "$UNIT" || exit 80

  cp "$V2_JOURNAL" "$V2_JOURNAL.valid"
  jq '.steps = [{"id":"unexpected-replay"}]' \
    "$V2_JOURNAL.valid" > "$V2_JOURNAL"
  set +e
  ( assert_activation_only_journal complete repair-transaction repair-authorization ) \
    >/dev/null 2>&1
  activation_journal_guard_rc=$?
  set -e
  [ "$activation_journal_guard_rc" -ne 0 ] || exit 81
  cp "$V2_JOURNAL.valid" "$V2_JOURNAL"

  cp "$V2_LEDGER" "$V2_LEDGER.valid"
  printf 'unexpected mutation\n' >> "$V2_LEDGER"
  set +e
  ( assert_activation_ledger_unchanged ) >/dev/null 2>&1
  activation_ledger_guard_rc=$?
  set -e
  [ "$activation_ledger_guard_rc" -ne 0 ] || exit 82
  cp "$V2_LEDGER.valid" "$V2_LEDGER"
)
set +e
run_activation_repair_contract success
activation_repair_rc=$?
set -e
[ "$activation_repair_rc" = 0 ] \
  || fail 'activation-only repair helper does not enforce its durable incident flow'
for activation_mutation in unit journal ledger; do
  set +e
  run_activation_repair_contract "unconfirmed-$activation_mutation-mutation" >/dev/null 2>&1
  activation_mutation_rc=$?
  set -e
  [ "$activation_mutation_rc" -ne 0 ] \
    || fail "activation-only repair helper accepted $activation_mutation mutation before confirmation"
done
assert_v2_transition_function="$(sed -n '/^assert_v2_transition() {/,/^}/p' \
  "$ROOT/dev/e2e/power-reconciler-upgrade.sh")"
(
  set -euo pipefail
  export OLD_VERSION=0.8.0
  OPERATOR_HOME="$TMP/power-rollback/operator"
  rollback_state="$OPERATOR_HOME/.config/subyard/release-transition/v2"
  rollback_target=0.8.0-bbbbbbbbbbbb
  CANDIDATE_RELEASE_TARGET=releases/0.11.3-aaaaaaaaaaaa
  runtime="$OPERATOR_HOME/.subyard/runtime"
  mkdir -p "$rollback_state" "$runtime/releases/$rollback_target" \
    "$runtime/$CANDIDATE_RELEASE_TARGET/config"
  printf 'published target manifest\n' > "$runtime/releases/$rollback_target/runtime-files.sha256"
  printf 'candidate owner registry\n' > "$runtime/$CANDIDATE_RELEASE_TARGET/config/release-transition.json"
  artifact_digest="$(sha256sum "$runtime/releases/$rollback_target/runtime-files.sha256" | awk '{print $1}')"
  registry_digest="$(sha256sum "$runtime/$CANDIDATE_RELEASE_TARGET/config/release-transition.json" | awk '{print $1}')"
  catalog_digest=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  jq -n '{
    schemaVersion: 2,
    domains: {
      "owner-registration": {epoch: 2, applied: ["canonicalize-test-yard-owner-v2"]},
      "power-metadata": {epoch: 1, applied: []},
      "project-state": {epoch: 1, applied: []},
      settings: {epoch: 2, applied: ["canonicalize-test-vms-settings-v2"]}
    }
  }' > "$rollback_state/ledger.json"
  # Published V2 shape: authorization/intent tokens use the frozen complete
  # rollback fixture in records_test.go; target facts are not durable fields.
  jq -n --arg target "$rollback_target" \
    --arg artifact "$artifact_digest" --arg registry "$registry_digest" --arg catalog "$catalog_digest" '{
    schemaVersion: 2, transaction: "tx-001", checkpoint: "complete",
    goal: {target: $target, direction: "activate-previous"},
    releases: {from: "0.11.3-aaaaaaaaaaaa", previous: $target, target: $target},
    authorizationPlan: "plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    resumePlan: "resume-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    artifactDigest: $artifact, registryDigest: $registry, catalogDigest: $catalog,
    observationScope: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    authorizationDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    intentDigest: "cffbd21069e88c37c6f18c83e9ac8643a98488e7b8be6ad5c8767c75eceef4b9",
    steps: []
  }' > "$rollback_state/journal.json"
  cp "$rollback_state/journal.json" "$rollback_state/journal.valid"
  eval "$assert_v2_transition_function"
  operator_env() { "$@"; }
  die() { return 2; }
  assert_v2_transition activate-previous "releases/$rollback_target" "$catalog_digest" \
    || fail 'power rollback rejected a completed frozen V2 journal'
  for rollback_mutation in wrong-target wrong-direction incomplete wrong-artifact wrong-registry \
    wrong-catalog missing-artifact missing-registry missing-catalog missing-steps unverified-step new-field; do
    case "$rollback_mutation" in
      wrong-target) filter='.goal.target = "foreign"' ;;
      wrong-direction) filter='.goal.direction = "activate-target"' ;;
      incomplete) filter='.checkpoint = "reconciling"' ;;
      wrong-artifact) filter='.artifactDigest = .catalogDigest' ;;
      wrong-registry) filter='.registryDigest = .catalogDigest' ;;
      wrong-catalog) filter='.catalogDigest = .artifactDigest' ;;
      missing-artifact) filter='del(.artifactDigest)' ;;
      missing-registry) filter='del(.registryDigest)' ;;
      missing-catalog) filter='del(.catalogDigest)' ;;
      missing-steps) filter='del(.steps)' ;;
      unverified-step) filter='.steps = [{checkpoint: "intent"}]' ;;
      new-field) filter='.rollbackTarget = {version: "0.8.0"}' ;;
    esac
    jq "$filter" "$rollback_state/journal.valid" > "$rollback_state/journal.json"
    set +e
    assert_v2_transition activate-previous "releases/$rollback_target" "$catalog_digest" >/dev/null 2>&1
    rollback_assert_rc=$?
    set -e
    [ "$rollback_assert_rc" -ne 0 ] \
      || fail "power rollback evidence accepted $rollback_mutation"
  done
) || fail 'power rollback journal lost frozen artifact and owner bindings'
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
      assert_candidate_state() { :; }
      assert_runtime_links() { :; }
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
  timeout_elapsed=$((SECONDS - started))
  timeout_child_state=''
  timeout_child_stopped=0
  for _ in {1..20}; do
    timeout_child_state="$(awk '{print $3}' "/proc/$child_pid/stat" 2>/dev/null || true)"
    case "$timeout_child_state" in
      ''|Z|X) timeout_child_stopped=1; break ;;
    esac
    sleep 0.1
  done
  [ "$timeout_rc" = 137 ] \
    && [ "$timeout_elapsed" -le 10 ] \
    && [ "$timeout_child_stopped" = 1 ] \
    || fail "TERM-ignoring timeout fixture or descendant escaped the KILL deadline: rc=$timeout_rc elapsed=${timeout_elapsed}s child_state=${timeout_child_state:-gone}"
  child_pid=''
)
reboot_vm_function="$(sed -n '/^reboot_vm() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-acceptance.sh")"
for reboot_scenario in transient empty degraded exhausted maintenance false-running; do
  reboot_probe_dir="$TMP/reboot-$reboot_scenario"
  mkdir -p "$reboot_probe_dir"
  printf '0\n' > "$reboot_probe_dir/clock"
  printf '0\n' > "$reboot_probe_dir/probes"
  set +e
  REBOOT_VM_FUNCTION="$reboot_vm_function" REBOOT_PROBE_DIR="$reboot_probe_dir" \
    REBOOT_SCENARIO="$reboot_scenario" bash -c '
      set -euo pipefail
      eval "$REBOOT_VM_FUNCTION"
      CONFIG=/fixture/ssh-config
      REBOOT_REQUEST_TIMEOUT_SECONDS=15
      POWER_RECONCILE_START_WAIT_SECONDS=30
      die() { printf "%s\n" "$*" >&2; exit 1; }
      p0_monotonic_seconds() { cat "$REBOOT_PROBE_DIR/clock"; }
      sleep() {
        local now
        now="$(cat "$REBOOT_PROBE_DIR/clock")"
        printf "%s\n" "$((now + $1))" > "$REBOOT_PROBE_DIR/clock"
      }
      timeout() {
        local bound
        while [[ "$1" = --* ]]; do shift; done
        bound="$1"
        shift
        if [[ "$*" = *is-system-running* ]]; then
          [ "$bound" -gt 0 ] && [ "$bound" -le 10 ] || exit 91
          [ "$bound" -le "$((180 - $(p0_monotonic_seconds)))" ] || exit 92
          [[ "$*" = *ConnectTimeout=3*ConnectionAttempts=1*ServerAliveInterval=2*ServerAliveCountMax=2* ]] || exit 93
          printf "%s\n" "$bound" >> "$REBOOT_PROBE_DIR/bounds"
        fi
        "$@"
      }
      ssh() {
        local probe
        case "$*" in
          *"sudo -n systemctl reboot") return 0 ;;
          *"-- true") return 255 ;;
          *boot_id*)
            if [ -e "$REBOOT_PROBE_DIR/boot-read" ]; then
              printf "new-boot\n"
            else
              touch "$REBOOT_PROBE_DIR/boot-read"
              printf "old-boot\n"
            fi ;;
          *is-system-running*)
            probe="$(cat "$REBOOT_PROBE_DIR/probes")"
            printf "%s\n" "$((probe + 1))" > "$REBOOT_PROBE_DIR/probes"
            case "$REBOOT_SCENARIO" in
              transient)
                [ "$probe" -gt 0 ] || return 255
                if [ "$probe" = 1 ]; then printf "starting\n"; return 1; fi
                printf "running\n" ;;
              empty) [ "$probe" -gt 0 ] || return 0; printf "running\n" ;;
              degraded) printf "degraded\n"; return 1 ;;
              exhausted) sleep "$bound"; return 255 ;;
              maintenance) printf "maintenance\n"; return 1 ;;
              false-running) printf "running\n"; return 255 ;;
            esac ;;
          *"ip -4 route show default") printf "default via fixture\n" ;;
          *) exit 94 ;;
        esac
      }
      boot_power_reconciler_succeeded() {
        touch "$REBOOT_PROBE_DIR/power-verified"
        return 0
      }
      reboot_vm 2
    ' > "$reboot_probe_dir/output" 2>&1
  reboot_probe_rc=$?
  set -e
  case "$reboot_scenario" in
    transient|empty|degraded)
      [ "$reboot_probe_rc" = 0 ] && [ -e "$reboot_probe_dir/power-verified" ] \
        && [ -s "$reboot_probe_dir/bounds" ] \
        || fail "P0 reboot rejected bounded terminal readiness: $reboot_scenario"
      if [ "$reboot_scenario" = transient ]; then
        [ "$(cat "$reboot_probe_dir/probes")" = 3 ] \
          || fail 'P0 reboot did not retry transport and starting observations'
      fi ;;
    *)
      [ "$reboot_probe_rc" != 0 ] && [ ! -e "$reboot_probe_dir/power-verified" ] \
        || fail "P0 reboot accepted unsafe host readiness: $reboot_scenario"
      grep -Fq 'probe_status=' "$reboot_probe_dir/output" \
        || fail 'P0 reboot failure omitted safe probe status diagnostics'
      if [ "$reboot_scenario" = exhausted ]; then
        [ "$(cat "$reboot_probe_dir/clock")" = 180 ] \
          || fail 'P0 reboot reset or exceeded the terminal readiness deadline'
      fi ;;
  esac
done
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
source_broker_wait="$(sed -n '/^wait_for_test_vm_broker() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh")"
(
  eval "$source_broker_wait"
  OPERATOR_HOME=/fixture/operator
  INSTANCE=yard-test-yard PROJECT=subyard-test-yard
  broker_hash="$(printf '%064d' 1)"
  broker_polls="$TMP/source-broker-polls"
  broker_ready_after=3
  operator_env() {
    [ "$*" = "sha256sum $OPERATOR_HOME/.subyard/runtime/current/bin/yard-engine" ] || return 2
    printf '%s  yard-engine\n' "$broker_hash"
  }
  incus() {
    [ "$*" = "exec $INSTANCE --project $PROJECT -- env WANT_ENABLED=1 WANT_ENGINE_HASH=$broker_hash /usr/local/libexec/subyard/test-vms-inner _test-vms-worker doctor" ] || return 2
    local poll
    poll=$(( $(cat "$broker_polls") + 1 ))
    printf '%s\n' "$poll" > "$broker_polls"
    [ "$poll" -lt "$broker_ready_after" ] || return 0
    printf 'inner Incus is inactive\n' >&2
    return 1
  }
  sleep() { [ "$1" = 1 ]; }
  printf '0\n' > "$broker_polls"
  wait_for_test_vm_broker || fail 'source fixture did not wait for broker readiness'
  [ "$(cat "$broker_polls")" = 3 ] || fail 'source fixture kept polling a ready broker'
  broker_ready_after=100
  printf '0\n' > "$broker_polls"
  if wait_for_test_vm_broker > "$TMP/source-broker-failure" 2>&1; then
    fail 'source fixture accepted a broker that never became ready'
  fi
  [ "$(cat "$broker_polls")" = 60 ] \
    && grep -Fxq 'inner Incus is inactive' "$TMP/source-broker-failure" \
    || fail 'source fixture lost its readiness bound or doctor diagnostic'
) || fail 'source fixture broker readiness regression'
source_finish="$(sed -n '/^finish() {/,/^}/p' "$ROOT/dev/e2e/p0-source-upgrade.sh")"
[[ "$source_finish" == *'operator_yard -Y "$YARD_NAME" start --yes'*'wait_for_test_vm_broker'*'rm /home/dev/.codex/rules/repo.rules'* ]] \
  || fail 'source fixture must establish broker readiness before seeding config drift'
(
  SOURCE_ROOT="$TMP/source-registration/src"
  OPERATOR_HOME="$TMP/source-registration/operator"
  mkdir -p "$SOURCE_ROOT/private/yards" "$OPERATOR_HOME/.config/subyard/yards/test-yard"
  original="$SOURCE_ROOT/private/yards/e2e-yard.env"
  migrated="$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env"
  printf '# retained comment\nYARD_TEMPLATE=e2e-vms\nSSH_PORT=2223\n' > "$original"
  sed 's/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/' "$original" > "$migrated"
  operator_env() { "$@"; }
  eval "$(sed -n '/^verify_migrated_yard_registration() {/,/^}/p' "$ROOT/dev/e2e/p0-source-upgrade.sh")"
  verify_migrated_yard_registration || fail 'initial source registration comparison failed'
  if verify_migrated_yard_registration adopted >/dev/null 2>&1; then
    fail 'adopted registration comparison accepted a missing selection'
  fi
  printf "CODING_TOOL_INTEGRATIONS=''\n" >> "$migrated"
  verify_migrated_yard_registration adopted || fail 'authorized empty adoption was rejected'
  if verify_migrated_yard_registration >/dev/null 2>&1; then
    fail 'pre-adoption comparison accepted a premature selection write'
  fi
  sed -i 's/^SSH_PORT=2223$/SSH_PORT=2224/' "$migrated"
  if verify_migrated_yard_registration adopted >/dev/null 2>&1; then
    fail 'adoption comparison accepted unrelated setting drift'
  fi
) || fail 'source-upgrade registration comparison did not retain exact migration evidence'
source_normalizer_function="$(sed -n '/^assert_direct_normalizer_is_pure() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh")"
run_source_normalizer_contract() (
  set -euo pipefail
  local scenario="$1"
  local fixture="$TMP/source-normalizer-$scenario"
  SOURCE_ROOT="$fixture/operator/src"
  OPERATOR_HOME="$fixture/operator"
  SHARED_ROOT="$fixture/shared"
  RELEASE_ROOT="$SHARED_ROOT/releases"
  CANDIDATE_A_REPOSITORY="$SHARED_ROOT/candidate-a-runtime"
  CANDIDATE_A_ENGINE="$CANDIDATE_A_REPOSITORY/bin/yard-engine"
  NORMALIZER_SCRATCH="$fixture/scratch"
  NORMALIZER_CALLS="$fixture/normalizer-calls"
  NORMALIZER_SCENARIO="$scenario"
  EXPECTED_CANDIDATE_REPOSITORY="$CANDIDATE_A_REPOSITORY"
  export EXPECTED_CANDIDATE_REPOSITORY NORMALIZER_CALLS NORMALIZER_SCENARIO \
    NORMALIZER_SCRATCH OPERATOR_HOME
  mkdir -p "$SOURCE_ROOT/bin" "$SOURCE_ROOT/private/yards" \
    "$CANDIDATE_A_REPOSITORY/bin" "$CANDIDATE_A_REPOSITORY/config" \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard" "$OPERATOR_HOME/.local/bin" \
    "$SHARED_ROOT" "$NORMALIZER_SCRATCH"
  printf 'ALPHA=1\nYARD_TEMPLATE=e2e-vms\nOMEGA=2\n' \
    > "$SOURCE_ROOT/private/yards/e2e-yard.env"
  : > "$NORMALIZER_CALLS"
  printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    '[ "${SUBYARD_REPOSITORY_ROOT:-}" = "$EXPECTED_CANDIDATE_REPOSITORY" ] || exit 65' \
    '[ -f "$SUBYARD_REPOSITORY_ROOT/config/commands.registry" ] || exit 66' \
    '[ "${1:-}" = _migrate ] && [ "${2:-}" = normalize-yard-config ] || exit 63' \
    'printf "invoke:%s\n" "$*" >> "$NORMALIZER_CALLS"' \
    'shift 2' \
    'if [ "$#" -ne 0 ]; then' \
    '  [ "$NORMALIZER_SCENARIO" != reject-stdout ] || printf "leaked rejected output\n"' \
    '  printf "path arguments rejected\n" >&2' \
    '  exit 64' \
    'fi' \
    'input="$NORMALIZER_SCRATCH/input"' \
    'cp /dev/stdin "$input"' \
    'bytes=$(wc -c < "$input")' \
    'printf "%s:read\n" "$bytes" >> "$NORMALIZER_CALLS"' \
    'if [ "$bytes" -gt 1048576 ] && [ "$NORMALIZER_SCENARIO" != oversized-accept ]; then' \
    '  if [ "$NORMALIZER_SCENARIO" = oversized-wrong-diagnostic ]; then' \
    '    printf "bash: oversized.env: Permission denied\n" >&2' \
    '  else' \
    '    printf "%s: source-install yard config normalization: legacy yard config exceeds its size bound\n" "${0##*/}" >&2' \
    '  fi' \
    '  exit 1' \
    'fi' \
    '[ "$NORMALIZER_SCENARIO" != mutate-tree ] || printf "mutated\n" > "$OPERATOR_HOME/.local/bin/unexpected"' \
    'if [ "$NORMALIZER_SCENARIO" = strip-newline ]; then' \
    '  normalized=$(sed "s/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/" "$input")' \
    '  printf "%s" "$normalized"' \
    'else' \
    '  sed "s/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/" "$input"' \
    'fi' \
    > "$CANDIDATE_A_ENGINE"
  printf 'fixture command registry\n' \
    > "$CANDIDATE_A_REPOSITORY/config/commands.registry"
  printf '%s\n' \
    '#!/bin/sh' \
    'printf "yard: internal: _migrate expects check or apply\n" >&2' \
    'exit 2' \
    > "$SOURCE_ROOT/bin/yard"
  chmod 0755 "$SOURCE_ROOT/bin/yard" "$CANDIDATE_A_ENGINE"
  eval "$source_normalizer_function"
  operator_env() {
    local argument
    for argument in "$@"; do
      if [ "$argument" = "$SHARED_ROOT/direct-normalizer-evidence/oversized.env" ]; then
        printf 'bash: %s: Permission denied\n' "$argument" >&2
        return 126
      fi
    done
    "$@"
  }
  die() { printf 'source normalizer fixture: %s\n' "$*" >&2; exit 2; }
  assert_direct_normalizer_is_pure
  [ "$(grep -c '^invoke:' "$NORMALIZER_CALLS")" = 3 ]
  grep -Fxq '1048577:read' "$NORMALIZER_CALLS"
)
run_source_normalizer_contract valid \
  || fail 'source normalizer helper rejected the exact pure boundary behavior'
for source_normalizer_scenario in strip-newline reject-stdout oversized-accept \
  oversized-wrong-diagnostic mutate-tree; do
  set +e
  run_source_normalizer_contract "$source_normalizer_scenario" >/dev/null 2>&1
  source_normalizer_rc=$?
  set -e
  [ "$source_normalizer_rc" -ne 0 ] \
    || fail "source normalizer helper missed $source_normalizer_scenario boundary drift"
done

source_ingress_function="$(sed -n '/^verify_authorized_source_ingress() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh")"
(
  set -euo pipefail
  OPERATOR_HOME="$TMP/source-ingress/operator"
  SOURCE_ROOT="$OPERATOR_HOME/src"
  V2_JOURNAL="$OPERATOR_HOME/.config/subyard/release-transition/v2/journal.json"
  mkdir -p "$(dirname "$V2_JOURNAL")"
  source_digest_a=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  source_digest_b=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  source_digest_c=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  source_digest_d=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  jq -n \
    --arg source "$SOURCE_ROOT" --arg data "$OPERATOR_HOME/.subyard" \
    --arg bin "$OPERATOR_HOME/.local/bin" --arg rc "$OPERATOR_HOME/.bashrc" \
    --arg login "$OPERATOR_HOME/.profile" \
    --arg a "$source_digest_a" --arg b "$source_digest_b" \
    --arg c "$source_digest_c" --arg d "$source_digest_d" '
      {
        schemaVersion: 2,
        transaction: "source-transaction",
        releases: {from: "source-old", target: "source-candidate"},
        checkpoint: "complete",
        sourceIngress: {
          schemaVersion: 1, kind: "pre-go-source-v1", sourceRoot: $source,
          dataHome: $data, binDir: $bin, rc: $rc, loginRC: $login
        },
        steps: [
          {
            id: "source-install.import", migration: "source-install-v1",
            resource: "source-install.config", decision: "canonicalize",
            expectedFingerprint: $a, desiredFingerprint: $b, checkpoint: "verified",
            evidence: {
              schemaVersion: 2, transaction: "source-transaction",
              releases: {from: "source-old", target: "source-candidate"},
              step: "source-install.import", expectedFingerprint: $a,
              desiredFingerprint: $b, observedFingerprint: $b, checkpoint: "verified"
            }
          },
          {
            id: "source-install.entrypoints", migration: "source-install-v1",
            resource: "source-install.entrypoints", decision: "canonicalize",
            expectedFingerprint: $c, desiredFingerprint: $d, checkpoint: "verified",
            evidence: {
              schemaVersion: 2, transaction: "source-transaction",
              releases: {from: "source-old", target: "source-candidate"},
              step: "source-install.entrypoints", expectedFingerprint: $c,
              desiredFingerprint: $d, observedFingerprint: $d, checkpoint: "verified"
            }
          }
        ]
      }
    ' > "$V2_JOURNAL"
  cp "$V2_JOURNAL" "$V2_JOURNAL.valid"
  eval "$source_ingress_function"
  operator_env() { "$@"; }
  die() { return 2; }
  verify_authorized_source_ingress
  for source_ingress_mutation in foreign-role wrong-step wrong-decision \
    wrong-evidence unexpected-recovery duplicate-step; do
    case "$source_ingress_mutation" in
      foreign-role)
        jq '.sourceIngress.dataHome += "-foreign"' "$V2_JOURNAL.valid" > "$V2_JOURNAL"
        ;;
      wrong-step)
        jq '.steps[0].resource = "source-install.foreign"' \
          "$V2_JOURNAL.valid" > "$V2_JOURNAL"
        ;;
      wrong-decision)
        jq '.steps[0].decision = "retain"' \
          "$V2_JOURNAL.valid" > "$V2_JOURNAL"
        ;;
      wrong-evidence)
        jq '.steps[0].evidence.observedFingerprint = .steps[0].expectedFingerprint' \
          "$V2_JOURNAL.valid" > "$V2_JOURNAL"
        ;;
      unexpected-recovery)
        jq '.steps[0].evidence.recoveryFingerprint =
          "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"' \
          "$V2_JOURNAL.valid" > "$V2_JOURNAL"
        ;;
      duplicate-step)
        jq '.steps += [.steps[0]]' "$V2_JOURNAL.valid" > "$V2_JOURNAL"
        ;;
    esac
    set +e
    verify_authorized_source_ingress >/dev/null 2>&1
    source_ingress_rc=$?
    set -e
    [ "$source_ingress_rc" -ne 0 ] \
      || fail "source ingress helper accepted $source_ingress_mutation journal evidence"
  done
) || fail 'P0 source-upgrade does not behaviorally verify its authorized source ingress'
consumer_fixture="$TMP/release-consumer"
consumer_registry="$consumer_fixture/routes"
consumer_log="$consumer_fixture/runner.log"
mkdir -p "$consumer_fixture/dev/e2e" "$consumer_registry/test-yard/current"
cp "$ROOT/dev/e2e/release-migration-consumer.sh" "$consumer_fixture/dev/e2e/"
touch "$consumer_registry/test-yard/current/route.tsv" \
  "$consumer_registry/test-yard/current/known_hosts"
git -C "$consumer_fixture" init -q
cat > "$consumer_fixture/dev/agent-e2e.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CONSUMER_RUNNER_LOG"
case "${1:-}" in
  --prepare) exit 0 ;;
  --status)
    [ "${2:-}" = --json ] || exit 91
    printf '%s\n' "$CONSUMER_BROKER_STATUS"
    ;;
  --slot)
    [ "${2:-}" = 3 ] || exit 92
    shift 2
    case "${1:-}" in
      --wait) [ "${2:-}" = 20m ] || exit 93 ;;
      *) exit 94 ;;
    esac
    ;;
  *) exit 95 ;;
esac
EOF
chmod +x "$consumer_fixture/dev/agent-e2e.sh"
consumer_status='{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":2,"lease_epoch":7,"state":"held"},{"slot_id":"slot-003","resource_generation":4,"lease_epoch":9,"state":"available"}]}}'
: > "$consumer_log"
SUBYARD_E2E_CONSUMER_FIXTURE=1 \
  SUBYARD_E2E_ROUTE_REGISTRY="$consumer_registry" \
  SUBYARD_E2E_SLOT=slot-001 \
  CONSUMER_RUNNER_LOG="$consumer_log" \
  CONSUMER_BROKER_STATUS="$consumer_status" \
  "$consumer_fixture/dev/e2e/release-migration-consumer.sh" \
  || fail 'release consumer rejected an available broker-local exact slot'
[ "$(sed -n '1p' "$consumer_log")" = --prepare ] \
  && [ "$(sed -n '2p' "$consumer_log")" = '--status --json' ] \
  && grep -Eq '^--slot 3 --wait 20m --vm both -- sh -c ' "$consumer_log" \
  && grep -Fxq -- '--slot 3 --wait 20m --verify-boundary' "$consumer_log" \
  && ! grep -Eq '^--slot 1([[:space:]]|$)' "$consumer_log" \
  || fail 'release consumer inherited its outer slot or omitted its broker-local exact slot'

for consumer_failure in no-available malformed-slot; do
  case "$consumer_failure" in
    no-available)
      failure_status='{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":2,"lease_epoch":7,"state":"held"}]}}'
      failure_message='no available broker-local slot'
      ;;
    malformed-slot)
      failure_status='{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-03","resource_generation":2,"lease_epoch":0,"state":"available"}]}}'
      failure_message='invalid broker-local slot id'
      ;;
  esac
  : > "$consumer_log"
  set +e
  consumer_failure_output="$(
    SUBYARD_E2E_CONSUMER_FIXTURE=1 \
      SUBYARD_E2E_ROUTE_REGISTRY="$consumer_registry" \
      SUBYARD_E2E_SLOT=slot-001 \
      CONSUMER_RUNNER_LOG="$consumer_log" \
      CONSUMER_BROKER_STATUS="$failure_status" \
      "$consumer_fixture/dev/e2e/release-migration-consumer.sh" 2>&1
  )"
  consumer_failure_rc=$?
  set -e
  [ "$consumer_failure_rc" = 2 ] \
    && grep -Fq "$failure_message" <<<"$consumer_failure_output" \
    && ! grep -Eq '^--slot [0-9]+ ' "$consumer_log" \
    || fail "release consumer did not fail closed for $consumer_failure"
done
grep -Fq 'select(.slot_id == $slot)' "$ROOT/dev/agent-e2e.sh" \
  && grep -Fq 'current lease slot is absent from pool status' "$ROOT/dev/agent-e2e.sh" \
  || fail "agent E2E boundary verification is coupled to unrelated concurrent slots"
! grep -Fq 'test-vms-inner' "$ROOT/dev/agent-e2e.sh" \
  || fail "agent E2E transport still invokes the privileged lifecycle worker"


printf 'ok: agent E2E lease transport is pinned, fenced and cleanup-owned\n'
