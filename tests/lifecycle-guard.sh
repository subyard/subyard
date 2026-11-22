#!/usr/bin/env bash
# Physical stop refuses active SSH sessions unless Go forces it or the plan approved the interruption.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=tests/helpers/test-lib.sh
. "$ROOT/tests/helpers/test-lib.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-lifecycle-guard.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
export MOCK_INCUS_LOG="$TMP/incus.log"
export MOCK_INCUS_STATE="$TMP/incus.state"
export MOCK_SYSTEMCTL_LOG="$TMP/systemctl.log"
cat > "$TMP/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info) exit 0 ;;
  list) [ "${*: -1}" != ns ] || printf '%s,' "$2"; cat "$MOCK_INCUS_STATE" ;;
  exec)
    case "$*" in
      *'systemctl is-active'*)
        printf 'ssh-snapshot\n' >> "$MOCK_INCUS_LOG"
        [ "${MOCK_QUIESCE_RC:-0}" = 0 ] || exit "$MOCK_QUIESCE_RC"
        printf '%s\n' "${MOCK_QUIESCE_STATE:-snapshot:1:0}"
        ;;
      *'systemctl stop'*)
        printf 'ssh-quiesce\n' >> "$MOCK_INCUS_LOG"
        [ "${MOCK_STOP_LISTENER_RC:-0}" = 0 ] || exit "$MOCK_STOP_LISTENER_RC"
        ;;
      *'systemctl start'*)
        printf 'ssh-restore\n' >> "$MOCK_INCUS_LOG"
        case "$*" in
          *'systemctl start ssh.socket'*'systemctl start ssh'*)
            printf 'ssh-restore-order-ok\n' >> "$MOCK_INCUS_LOG"
            ;;
          *) printf 'ssh-restore-order-bad\n' >> "$MOCK_INCUS_LOG" ;;
        esac
        [ "${MOCK_RESTORE_RC:-0}" = 0 ] || exit "$MOCK_RESTORE_RC"
        ;;
      *'command -v python3'*)
        printf '%s\n' "${MOCK_PROVISIONED:-provisioned}"
        ;;
      *'check-ssh'*)
        printf 'all-user-ssh-probe\n' >> "$MOCK_INCUS_LOG"
        case "$*" in *VSCODE_USER*) exit 91 ;; esac
        printf '%s\n' "${MOCK_VSCODE_STATE:-idle}"
        ;;
      *)
        printf 'vscode-probe\n' >> "$MOCK_INCUS_LOG"
        [ "${MOCK_VSCODE_RC:-0}" = 0 ] || exit "$MOCK_VSCODE_RC"
        printf '%s\n' "${MOCK_VSCODE_STATE:-idle}"
        ;;
    esac
    ;;
  stop)
    printf 'stop\n' >> "$MOCK_INCUS_LOG"
    printf 'STOPPED\n' > "$MOCK_INCUS_STATE"
    ;;
  config)
    [ "${2:-}" = get ] && [ -n "${MOCK_INCUS_CONFIG:-}" ] || exit 90
    # MOCK_INCUS_CONFIG is "managed initialized ever_initialized"; "-" is unset.
    read -r managed initialized ever <<<"$MOCK_INCUS_CONFIG"
    case "$4" in
      user.subyard.managed) value=$managed ;;
      user.subyard.initialized) value=$initialized ;;
      user.subyard.ever_initialized) value=$ever ;;
      *) exit 90 ;;
    esac
    [ "$value" = - ] || printf '%s\n' "$value"
    ;;
  *) exit 90 ;;
esac
SH
chmod +x "$TMP/bin/incus"
# Synthetic profile handler keeps this test about lifecycle ordering and recovery.
cat > "$TMP/bin/fixture-owner" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$(cat "$MOCK_PROFILE_MARKER")" = managed ] || exit 0
case "$1" in
  --pause)
    # A guest-only owner has nothing to pause in a guest init never provisioned.
    [ "${MOCK_OWNER_GUEST_ONLY:-0}" = 1 ] && [ "${SUBYARD_GUEST_NEVER_PROVISIONED:-0}" = 1 ] && exit 0
    [ "${MOCK_PROFILE_PAUSE_RC:-0}" = 0 ] || exit "$MOCK_PROFILE_PAUSE_RC"
    [ "$(cat "$MOCK_PROFILE_STATE_FILE")" = active ] || exit 0
    printf 'profile-stop\n' >> "$MOCK_INCUS_LOG"
    printf 'inactive\n' > "$MOCK_PROFILE_STATE_FILE"
    printf 'paused\n'
    ;;
  --resume)
    printf 'profile-start\n' >> "$MOCK_INCUS_LOG"
    printf 'active\n' > "$MOCK_PROFILE_STATE_FILE"
    ;;
  *) exit 2 ;;
esac
SH
cat > "$TMP/bin/engine" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = _profile-services ] || exit 2
case "$3" in
  --pause)
    paused="$(fixture-owner --pause)"
    [ -z "$paused" ] || printf 'fixture\n'
    ;;
  --resume)
    [ "${4:-}" = fixture ] || exit 2
    fixture-owner --resume
    ;;
  *) exit 2 ;;
esac
SH
chmod +x "$TMP/bin/fixture-owner" "$TMP/bin/engine"
export SUBYARD_DISPATCHER_PATH="$TMP/bin/engine"
export PATH="$TMP/bin:$PATH"
# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP" test-project test-yard
export SUBYARD_CONFIG_LOADED=1
export SUBYARD_ENGINE_CONTEXT=1
export SUBYARD_NO_AUDIT=1
export SSH_HOST=yard-test
export PROG=yard
SUBYARD_USER="$(id -un)"
export SUBYARD_USER
export SUBYARD_YARD=test-yard
export MOCK_PROFILE_STATE_FILE="$TMP/profile.state"
export MOCK_PROFILE_MARKER="$TMP/profile-marker"
printf 'managed\n' > "$MOCK_PROFILE_MARKER"

reset_case() {
  printf 'RUNNING\n' > "$MOCK_INCUS_STATE"
  : > "$MOCK_INCUS_LOG"
  : > "$MOCK_SYSTEMCTL_LOG"
  export MOCK_PROFILE_STATE=active
  printf 'active\n' > "$MOCK_PROFILE_STATE_FILE"
  export MOCK_VSCODE_STATE=idle MOCK_VSCODE_RC=0 MOCK_QUIESCE_RC=0
  export MOCK_STOP_LISTENER_RC=0 MOCK_RESTORE_RC=0
  export MOCK_QUIESCE_STATE=snapshot:1:0
  unset MOCK_INCUS_CONFIG MOCK_PROFILE_PAUSE_RC MOCK_PROVISIONED MOCK_OWNER_GUEST_ONLY SUBYARD_GUEST_NEVER_PROVISIONED
}

reset_case
MOCK_VSCODE_STATE=active
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "stop accepted an active VS Code window"
fi
grep -Fq 'Close Remote Connection' "$TMP/out" \
  || fail "active-window failure has no repair guidance: $(tr '\n' ' ' < "$TMP/out")"
grep -Fq "ssh-agent lock" "$TMP/out" \
  || fail "active-session failure does not name the SSH agent forwarder: $(tr '\n' ' ' < "$TMP/out")"
if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "active-window guard reached incus stop"; fi
grep -Fxq ssh-restore "$MOCK_INCUS_LOG" || fail "blocked stop did not restore the SSH listener"
grep -Fxq ssh-restore-order-ok "$MOCK_INCUS_LOG" \
  || fail "SSH socket was not restored before the service"
grep -Fxq profile-stop "$MOCK_INCUS_LOG" || fail "active-window stop did not pause the profile"
grep -Fxq profile-start "$MOCK_INCUS_LOG" || fail "active-window refusal did not resume the profile"
profile_start_line="$(grep -nFx profile-start "$MOCK_INCUS_LOG" | cut -d: -f1)"
restore_line="$(grep -nFx ssh-restore "$MOCK_INCUS_LOG" | cut -d: -f1)"
[ "$restore_line" -lt "$profile_start_line" ] \
  || fail "active-window refusal resumed the profile before restoring SSH"
reset_case
MOCK_VSCODE_STATE=active
"$ROOT/scripts/lifecycle-guard.sh" stop --force > "$TMP/out" 2>&1
grep -Fq 'bypasses the active SSH / VS Code session guard' "$TMP/out" \
  || fail "forced active stop emitted no warning"
grep -Fxq stop "$MOCK_INCUS_LOG" || fail "--force did not reach incus stop"
reset_case
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1
probe_line="$(grep -nFx vscode-probe "$MOCK_INCUS_LOG" | cut -d: -f1)"
quiesce_line="$(grep -nFx ssh-quiesce "$MOCK_INCUS_LOG" | cut -d: -f1)"
snapshot_line="$(grep -nFx ssh-snapshot "$MOCK_INCUS_LOG" | cut -d: -f1)"
stop_line="$(grep -nFx stop "$MOCK_INCUS_LOG" | cut -d: -f1)"
profile_stop_line="$(grep -nFx profile-stop "$MOCK_INCUS_LOG" | cut -d: -f1)"
profile_start_line="$(grep -nFx profile-start "$MOCK_INCUS_LOG" | cut -d: -f1)"
if [ "$snapshot_line" -ge "$quiesce_line" ] || [ "$quiesce_line" -ge "$profile_stop_line" ] \
    || [ "$profile_stop_line" -ge "$probe_line" ] || [ "$probe_line" -ge "$stop_line" ] \
    || [ "$stop_line" -ge "$profile_start_line" ]; then
  fail "idle stop did not run quiesce -> profile pause -> probe -> stop -> profile resume"
fi

reset_case
MOCK_VSCODE_RC=9
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "stop accepted an unknown VS Code state"
fi
grep -Fq 'could not verify' "$TMP/out" || fail "unknown-state failure is unclear"
if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "unknown-state guard reached incus stop"; fi
grep -Fxq ssh-restore "$MOCK_INCUS_LOG" || fail "unknown-state stop did not restore SSH"
grep -Fxq profile-stop "$MOCK_INCUS_LOG" || fail "unknown-state stop did not pause the profile"
grep -Fxq profile-start "$MOCK_INCUS_LOG" || fail "unknown-state stop did not resume the profile"

# An operator-approved restart keeps the fence and probe but reports live sessions.
for approved in active unknown idle; do
  reset_case
  if [ "$approved" = unknown ]; then MOCK_VSCODE_RC=9; else MOCK_VSCODE_STATE="$approved"; fi
  "$ROOT/scripts/lifecycle-guard.sh" stop --interrupt-sessions > "$TMP/out" 2>&1 \
    || fail "approved stop refused $approved sessions: $(tr '\n' ' ' < "$TMP/out")"
  grep -Fxq stop "$MOCK_INCUS_LOG" || fail "approved stop with $approved sessions did not reach incus stop"
  grep -Fxq ssh-quiesce "$MOCK_INCUS_LOG" || fail "approved stop skipped the SSH listener fence"
  grep -Fxq vscode-probe "$MOCK_INCUS_LOG" || fail "approved stop skipped the session probe"
  grep -Fxq profile-stop "$MOCK_INCUS_LOG" || fail "approved stop did not pause the profile"
  if grep -Fxq ssh-restore "$MOCK_INCUS_LOG"; then fail "approved stop restored the SSH listener"; fi
  case "$approved" in
    active) grep -Fq 'interrupting the SSH and VS Code Remote-SSH sessions' "$TMP/out" \
      || fail "approved stop did not report interrupted sessions" ;;
    unknown) grep -Fq 'could not verify SSH and VS Code sessions' "$TMP/out" \
      || fail "approved stop did not report the unknown session state" ;;
    idle) if grep -Fq '[warn]' "$TMP/out"; then fail "approved idle stop warned: $(tr '\n' ' ' < "$TMP/out")"; fi ;;
  esac
done
for misuse in "start --interrupt-sessions" "stop --interrupt-sessions --force" "stop --interrupt-sessions --reconcile"; do
  reset_case
  # shellcheck disable=SC2086 # The case lists whole argument vectors.
  if "$ROOT/scripts/lifecycle-guard.sh" $misuse > "$TMP/out" 2>&1; then
    fail "guard accepted '$misuse'"
  fi
  grep -Fq 'needs an ordinary stop' "$TMP/out" || fail "'$misuse' refusal is unclear"
  if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "'$misuse' reached incus stop"; fi
done

reset_case
MOCK_STOP_LISTENER_RC=9
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "stop accepted a failed SSH-listener quiescence"
fi
grep -Fq 'could not pause new SSH connections' "$TMP/out" \
  || fail "listener-quiescence failure is unclear"
grep -Fxq ssh-restore "$MOCK_INCUS_LOG" \
  || fail "partial listener quiescence did not restore the original state"
if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "failed listener quiescence reached incus stop"; fi

reset_case
MOCK_QUIESCE_STATE=unsupported
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "stop accepted an unsupported SSH KillMode"
fi
grep -Fq 'KillMode' "$TMP/out" || fail "unsupported SSH listener failure is unclear"
if grep -Fxq ssh-quiesce "$MOCK_INCUS_LOG"; then fail "unsupported listener was stopped"; fi

reset_case
MOCK_QUIESCE_RC=9
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "stop accepted unavailable listener state"
fi
if grep -Fxq ssh-quiesce "$MOCK_INCUS_LOG"; then fail "unavailable listener was stopped"; fi

reset_case
MOCK_VSCODE_STATE=active
"$ROOT/scripts/lifecycle-guard.sh" stop --reconcile > "$TMP/out" 2>&1
grep -Fxq stop "$MOCK_INCUS_LOG" || fail "reconcile stop did not reach incus stop"
if grep -Fxq ssh-snapshot "$MOCK_INCUS_LOG"; then fail "reconcile stop entered the session guard"; fi

reset_case
printf 'unmanaged\n' > "$MOCK_PROFILE_MARKER"
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1
if grep -Fxq profile-stop "$MOCK_INCUS_LOG" || grep -Fxq profile-start "$MOCK_INCUS_LOG"; then
  fail "unmanaged profile service was mutated"
fi
printf 'managed\n' > "$MOCK_PROFILE_MARKER"

reset_case
export MOCK_PROFILE_STATE=inactive
printf 'inactive\n' > "$MOCK_PROFILE_STATE_FILE"
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1
if grep -Fxq profile-stop "$MOCK_INCUS_LOG" || grep -Fxq profile-start "$MOCK_INCUS_LOG"; then
  fail "inactive profile service was mutated"
fi

printf 'STOPPED\n' > "$MOCK_INCUS_STATE"
: > "$MOCK_INCUS_LOG"
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1
if grep -Fxq vscode-probe "$MOCK_INCUS_LOG"; then fail "already-stopped yard ran the VS Code probe"; fi

# A failed first init can already have installed owner services, such as a host
# broker whose transport holds an SSH session: stop pauses every installed one.
# In a guest init never provisioned, a guest-only owner has nothing to pause and
# the session check covers all users, since the developer account may not exist.
reset_case
export MOCK_INCUS_CONFIG="true false -" MOCK_PROVISIONED=unprovisioned
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1 \
  || fail "never-provisioned stop with an installed host owner refused: $(tr '\n' ' ' < "$TMP/out")"
grep -Fxq profile-stop "$MOCK_INCUS_LOG" || fail "never-provisioned stop did not pause the installed owner"
grep -Fxq profile-start "$MOCK_INCUS_LOG" || fail "never-provisioned stop did not resume the installed owner"
grep -Fxq ssh-quiesce "$MOCK_INCUS_LOG" || fail "never-provisioned stop skipped the SSH listener fence"
grep -Fxq all-user-ssh-probe "$MOCK_INCUS_LOG" || fail "never-provisioned stop skipped the all-user session probe"
if grep -Fxq vscode-probe "$MOCK_INCUS_LOG"; then fail "never-provisioned stop probed the developer account"; fi
grep -Fxq stop "$MOCK_INCUS_LOG" || fail "never-provisioned stop did not reach incus stop"
reset_case
export MOCK_INCUS_CONFIG="true false -" MOCK_PROVISIONED=unprovisioned MOCK_OWNER_GUEST_ONLY=1 MOCK_PROFILE_PAUSE_RC=2
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1 \
  || fail "never-provisioned stop with a guest-only owner refused: $(tr '\n' ' ' < "$TMP/out")"
if grep -Fxq profile-stop "$MOCK_INCUS_LOG"; then fail "guest-only owner paused in a never-provisioned guest"; fi
reset_case
export MOCK_INCUS_CONFIG="true false -" MOCK_PROVISIONED=unprovisioned MOCK_VSCODE_STATE=active
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "never-provisioned stop crossed an active SSH session"
fi
if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "never-provisioned active-session guard reached incus stop"; fi
# A provisioned guest, a working yard whose re-init failed and a yard from a
# release before the completion mark pause strictly.
for config in "true false -:provisioned" "true false true:unprovisioned" "true true -:unprovisioned"; do
  reset_case
  export MOCK_INCUS_CONFIG="${config%%:*}" MOCK_PROVISIONED="${config#*:}" MOCK_OWNER_GUEST_ONLY=1 MOCK_PROFILE_PAUSE_RC=2
  if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
    fail "stop with '$config' accepted a failed owner-service pause"
  fi
  grep -Fq 'could not pause profile owner services' "$TMP/out" || fail "pause failure with '$config' is unclear"
  if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "failed pause with '$config' reached incus stop"; fi
done

printf 'ok: yard stop protects active VS Code Remote-SSH state\n'
