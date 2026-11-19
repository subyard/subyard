#!/usr/bin/env bash
# Check OpenClaw acceptance dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
manifest="$ROOT/config/profiles/openclaw/tests/e2e/external-obligations.json"
jq -e '.schema_version == 1 and (.obligations | length >= 1) and all(.obligations[]; (.id | type == "string" and length > 0) and (.description | type == "string" and length > 0) and (.required_inputs | type == "array" and length > 0))' \
  "$manifest" >/dev/null
# shellcheck source=tests/helpers/profile-controller.sh
. "$ROOT/tests/helpers/profile-controller.sh"
setup_profile_controller "$ROOT" config/profiles/openclaw/tests/e2e/acceptance.sh
assert_controller_missing_slot
assert_controller_rejects 2 --slot 0
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --vm-count 1 --purpose openclaw-provision --vm 1 -- bash config/profiles/openclaw/tests/e2e/owner.sh' "$tmp/calls"
assert_controller_failure --slot 7
printf 'ok: OpenClaw acceptance dispatch, slot requirement and failure propagation\n'

# Capture must precede cleanup and contain only fixed labels, even on native failure.
owner="$ROOT/config/profiles/openclaw/tests/e2e/owner.sh"
assert_owner_yard_sg_arguments "$owner"
diagnostic_fixture="$tmp/subyard-openclaw.failure"
install -d -m 0700 "$diagnostic_fixture"
printf 'openclaw-profile-e2e-v1\n' > "$diagnostic_fixture/.marker"
{
  printf 'set -euo pipefail\nSTATE=%q\nTOKEN=\nYARD_NAME=\n' "$diagnostic_fixture"
  printf 'STAGING_TRACE_DIR=\nSTAGING_ORIGINAL_PATH=\nSTAGING_CAPTURE_ACTIVE=0\nSTAGING_STEP=unknown\nSTAGING_STEP_STATE=unknown\n'
  for helper in enable_staging_capture staging_step capture_staging_failure; do
    sed -n "/^$helper() {/,/^}/p" "$owner"
  done
  sed -n '/^cleanup() {/,/^}/p' "$owner" | sed "s|/var/tmp/subyard-openclaw\.|$tmp/subyard-openclaw.|"
  cat <<'FAILURE_FIXTURE'
sudo() { shift; "$@"; }
trap cleanup EXIT
enable_staging_capture
[ "$(stat -c %a "$STAGING_TRACE_DIR")" = 700 ]
[ "$(stat -c %a "$STAGING_TRACE_DIR/step")" = 600 ]
yard() { printf 'synthetic-secret-must-not-leak\n'; return 42; }
staging_step first-start "$STATE/staging-start.out" start --yes
FAILURE_FIXTURE
} > "$tmp/failure.sh"
if bash "$tmp/failure.sh" > "$tmp/failure.out" 2>&1; then
  printf 'FAIL: diagnostic fixture concealed the native failure\n' >&2; exit 1
else
  [ "$?" = 42 ] || { cat "$tmp/failure.out" >&2; exit 1; }
fi
[ ! -d "$diagnostic_fixture" ]
grep -Fq 'openclaw_e2e_failure original_rc=42 last_step=first-start step_state=start' "$tmp/failure.out"
grep -Fq 'native_match=unknown capture_rc=0' "$tmp/failure.out"
grep -Fq 'original_rc=42 capture_rc=0 cleanup_rc=0' "$tmp/failure.out"
! grep -Fq synthetic-secret "$tmp/failure.out"
install -d -m 0700 "$diagnostic_fixture"
printf 'openclaw-profile-e2e-v1\n' > "$diagnostic_fixture/.marker"
sed 's/return 42/return 0/' "$tmp/failure.sh" > "$tmp/success.sh"
bash "$tmp/success.sh" > "$tmp/success.out" 2>&1
[ ! -d "$diagnostic_fixture" ]
grep -Fq 'original_rc=0 capture_rc=0 cleanup_rc=0' "$tmp/success.out"
! grep -Fq openclaw_e2e_failure "$tmp/success.out"
! grep -Fq synthetic-secret "$tmp/success.out"
# A capture failure is reported separately and cannot replace the original failure.
install -d -m 0700 "$diagnostic_fixture"
printf 'openclaw-profile-e2e-v1\n' > "$diagnostic_fixture/.marker"
sed '/staging_step first-start/i printf "invalid\\n" > "$STAGING_TRACE_DIR/probe.0"; chmod 0644 "$STAGING_TRACE_DIR/probe.0"' \
  "$tmp/failure.sh" > "$tmp/capture-failure.sh"
if bash "$tmp/capture-failure.sh" > "$tmp/capture-failure.out" 2>&1; then exit 1; else [ "$?" = 42 ]; fi
[ ! -d "$diagnostic_fixture" ]
grep -Fq 'original_rc=42 capture_rc=2 cleanup_rc=0' "$tmp/capture-failure.out"
install -d -m 0700 "$diagnostic_fixture"
printf 'openclaw-profile-e2e-v1\n' > "$diagnostic_fixture/.marker"
sed 's/sudo() { shift; "$@"; }/sudo() { return 1; }/' "$tmp/failure.sh" > "$tmp/cleanup-failure.sh"
if bash "$tmp/cleanup-failure.sh" > "$tmp/cleanup-failure.out" 2>&1; then exit 1; else [ "$?" = 42 ]; fi
grep -Fq 'original_rc=42 capture_rc=0 cleanup_rc=3' "$tmp/cleanup-failure.out"

# A fatal ownership refusal leaves the foreign tree intact and still tears down the owned yard.
install -d -m 0700 "$tmp/subyard-p0-project-foreign" "$diagnostic_fixture/config/yards/owned"
printf 'openclaw-profile-e2e-v1\n' > "$diagnostic_fixture/.marker"
printf 'fixture\n' > "$diagnostic_fixture/config/yards/owned/config.env"
{
  printf 'set -euo pipefail\nSTATE=%q\nSUBYARD_CONFIG_HOME=%q\nTOKEN=foreign\nYARD_NAME=owned\nMARKER=owned\n' \
    "$diagnostic_fixture" "$diagnostic_fixture/config"
  printf 'STAGING_ORIGINAL_PATH=\nSTAGING_CAPTURE_ACTIVE=0\n'
  sed -n '/^project_contract_clean_tree() {/,/^}/p' "$ROOT/dev/e2e/lib-owner-project-contract.sh" \
    | sed "s|/tmp/subyard-p0-project-|$tmp/subyard-p0-project-|g"
  sed -n '/^cleanup() {/,/^}/p' "$owner" \
    | sed "s|/var/tmp/subyard-openclaw\.|$tmp/subyard-openclaw.|; s|/tmp/subyard-p0-project-|$tmp/subyard-p0-project-|g"
  cat <<'REFUSED_CLEANUP'
die() { printf '%s\n' "$1" >&2; exit 2; }
sudo() { shift; "$@"; }
yard() { printf '%s\n' "$*" > "$STATE/../owned-teardown"; }
trap cleanup EXIT
exit 42
REFUSED_CLEANUP
} > "$tmp/refused-cleanup.sh"
rc=0
bash "$tmp/refused-cleanup.sh" > "$tmp/refused-cleanup.out" 2>&1 || rc=$?
[ "$rc" = 42 ]
[ -d "$tmp/subyard-p0-project-foreign" ]
[ ! -d "$diagnostic_fixture" ]
grep -Fxq 'teardown --yes' "$tmp/owned-teardown"
grep -Fxq 'refusing to clean an unmarked project fixture' "$tmp/refused-cleanup.out"
grep -Fq 'original_rc=42 capture_rc=0 cleanup_rc=3' "$tmp/refused-cleanup.out"

# Each launch gets one attempt, and a busy refusal is accepted only alongside a success.
(
  # shellcheck source=dev/e2e/lib-owner-project-contract.sh
  . "$ROOT/dev/e2e/lib-owner-project-contract.sh"
  busy_release_gate() {
    printf '%s\n' \
      'yard: release gate: release operator-action-required (plan-stale): active fixture; this command is not run' \
      'yard: cause: release state is changing; retry' >&2
    return 1
  }
  sync_once() {
    printf 'launch\n' >> "$case_dir/launches"
    if mkdir "$case_dir/first-sync" 2>/dev/null; then
      case "$scenario" in
        one-busy|both-busy) busy_release_gate; return ;;
        other-failure) printf 'yard: a different plan-stale refusal\n' >&2; return 42 ;;
      esac
    fi
    [ "$scenario" != both-busy ] || { busy_release_gate; return; }
    sleep 0.1
    touch "$case_dir/success-finished"
  }
  for scenario in two-successes one-busy both-busy other-failure; do
    case_dir="$tmp/pair-$scenario"
    mkdir "$case_dir"
    rc=0
    copies="$(project_contract_sync_pair "$case_dir" sync_once 2>"$case_dir/stderr")" || rc=$?
    [ "$(wc -l < "$case_dir/launches")" = 2 ]
    case "$scenario" in
      two-successes) [ "$rc:$copies" = 0:2 ] || exit 1 ;;
      one-busy) [ "$rc:$copies" = 0:1 ] || exit 1 ;;
      both-busy) [ "$rc:$copies" = 1: ] && [ ! -e "$case_dir/success-finished" ] || exit 1 ;;
      other-failure) [ "$rc:$copies" = 42: ] && [ -f "$case_dir/success-finished" ] || exit 1 ;;
    esac
  done
  grep -Fxq 'concurrent project sync failed (exits=1,1)' "$tmp/pair-both-busy/stderr"
  grep -Fxq 'yard: a different plan-stale refusal' "$tmp/pair-other-failure/stderr"
)

# The delegate itself must preserve bytes, exit and process-group cancellation.
(
  for helper in enable_staging_capture capture_staging_failure; do
    eval "$(sed -n "/^$helper() {/,/^}/p" "$owner")"
  done
  STATE="$tmp/delegate"
  install -d -m 0700 "$STATE"
  enable_staging_capture
  printf 'first-start\n' > "$STAGING_TRACE_DIR/step"
  cat > "$tmp/native" <<'NATIVE_FIXTURE'
#!/usr/bin/env bash
if [ "${1:-}" = cancel ]; then
  printf '%s\n' "$$" > "$MOCK_NATIVE_ROOT/child.pid"
  exec sleep 60
fi
printf '%s\0' "$@" > "$MOCK_NATIVE_ROOT/argv"
cat > "$MOCK_NATIVE_ROOT/stdin"
printf 'native-output\000bytes\n'
printf 'native-error\000bytes\n' >&2
exit 31
NATIVE_FIXTURE
  chmod 0700 "$tmp/native"
  sed "s|/usr/bin/incus|$tmp/native|" "$STAGING_TRACE_DIR/bin/incus" > "$tmp/delegate-script"
  cat "$tmp/delegate-script" > "$STAGING_TRACE_DIR/bin/incus"
  export MOCK_NATIVE_ROOT="$tmp"
  printf 'synthetic-secret-input\000bytes\n' > "$tmp/input"
  printf '%s\0' exec fixture -- docker inspect synthetic-secret-argument > "$tmp/argv.expected"
  if "$STAGING_TRACE_DIR/bin/incus" exec fixture -- docker inspect synthetic-secret-argument \
    < "$tmp/input" > "$tmp/stdout" 2> "$tmp/stderr"; then exit 1; else [ "$?" = 31 ]; fi
  cmp "$tmp/argv.expected" "$tmp/argv"
  cmp "$tmp/input" "$tmp/stdin"
  printf 'native-output\000bytes\n' | cmp - "$tmp/stdout"
  printf 'native-error\000bytes\n' | cmp - "$tmp/stderr"
  # Exercise reuse of the fixed slots without growing the trace or changing native status.
  for ((probe=0; probe<140; probe++)); do
    if "$STAGING_TRACE_DIR/bin/incus" info </dev/null >/dev/null 2>&1; then exit 1; else [ "$?" = 31 ]; fi
  done
  [ "$(find "$STAGING_TRACE_DIR" -name 'probe.*' -type f | wc -l)" -le 128 ]
  [ "$(find "$STAGING_TRACE_DIR" -name 'probe.*' ! -perm 0600 | wc -l)" = 0 ]
  [ "$(find "$STAGING_TRACE_DIR" -name 'probe.*' -size +256c | wc -l)" = 0 ]
  ! grep -Fq synthetic-secret "$STAGING_TRACE_DIR"/probe.*
  (
    setsid "$STAGING_TRACE_DIR/bin/incus" cancel </dev/null > "$tmp/cancel.out" 2>&1 &
    group=$!
    trap 'kill -KILL -- "-$group" 2>/dev/null || true' EXIT
    for ((probe=0; probe<100; probe++)); do [ ! -f "$tmp/child.pid" ] || break; sleep .01; done
    [ -f "$tmp/child.pid" ]
    child="$(cat "$tmp/child.pid")"
    kill -KILL -- "-$group"
    wait "$group" 2>/dev/null && exit 1
    if state="$(cat "/proc/$child/stat" 2>/dev/null)"; then
      state="${state##*) }"
      [ "${state%% *}" = Z ] || [ "${state%% *}" = X ]
    fi
  )
  # shellcheck disable=SC2034 # consumed by the extracted capture helper
  STAGING_STEP=first-start STAGING_STEP_STATE=start
  capture_staging_failure 137 > "$tmp/cancel-capture.out"
  grep -Fq 'native_match=pending' "$tmp/cancel-capture.out"
  ! grep -Fq synthetic-secret "$tmp/cancel-capture.out"
)
printf 'ok: OpenClaw pre-cleanup diagnostics preserve native behavior and bounded redacted evidence\n'
