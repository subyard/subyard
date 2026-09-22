#!/usr/bin/env bash
# Physical stop refuses active SSH sessions unless Go explicitly forces it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

mkdir -p "$TMP/bin"
export MOCK_INCUS_LOG="$TMP/incus.log"
export MOCK_INCUS_STATE="$TMP/incus.state"
export MOCK_SYSTEMCTL_LOG="$TMP/systemctl.log"
cat > "$TMP/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info) exit 0 ;;
  list) cat "$MOCK_INCUS_STATE" ;;
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
  *) exit 90 ;;
esac
SH
chmod +x "$TMP/bin/incus"
cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  --user)
    shift
    case "${1:-}" in
      show)
        printf 'broker-show\n' >> "$MOCK_INCUS_LOG"
        if [ "${MOCK_BROKER_STATE:-inactive}" = active ]; then
          printf 'active\n'
        else
          printf 'inactive\n'
        fi
        ;;
      stop)
        printf 'broker-stop\n' >> "$MOCK_INCUS_LOG"
        printf 'stop\n' >> "$MOCK_SYSTEMCTL_LOG"
        printf 'inactive\n' > "$MOCK_BROKER_STATE_FILE"
        ;;
      start)
        printf 'broker-start\n' >> "$MOCK_INCUS_LOG"
        printf 'start\n' >> "$MOCK_SYSTEMCTL_LOG"
        printf 'active\n' > "$MOCK_BROKER_STATE_FILE"
        ;;
      *) exit 0 ;;
    esac
    ;;
  *) exit 0 ;;
esac
SH
chmod +x "$TMP/bin/systemctl"
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
export MOCK_BROKER_STATE_FILE="$TMP/broker.state"
mkdir -p "$SUBYARD_OPERATOR_HOME/.config/systemd/user"
printf '# Managed by Subyard GitHub broker\n' \
  > "$SUBYARD_OPERATOR_HOME/.config/systemd/user/subyard-github-test-yard.service"

reset_case() {
  printf 'RUNNING\n' > "$MOCK_INCUS_STATE"
  : > "$MOCK_INCUS_LOG"
  : > "$MOCK_SYSTEMCTL_LOG"
  export MOCK_BROKER_STATE=active
  printf 'active\n' > "$MOCK_BROKER_STATE_FILE"
  export MOCK_VSCODE_STATE=idle MOCK_VSCODE_RC=0 MOCK_QUIESCE_RC=0
  export MOCK_STOP_LISTENER_RC=0 MOCK_RESTORE_RC=0
  export MOCK_QUIESCE_STATE=snapshot:1:0
}

reset_case
MOCK_VSCODE_STATE=active
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "stop accepted an active VS Code window"
fi
grep -Fq 'Close Remote Connection' "$TMP/out" \
  || fail "active-window failure has no repair guidance: $(tr '\n' ' ' < "$TMP/out")"
if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "active-window guard reached incus stop"; fi
grep -Fxq ssh-restore "$MOCK_INCUS_LOG" || fail "blocked stop did not restore the SSH listener"
grep -Fxq ssh-restore-order-ok "$MOCK_INCUS_LOG" \
  || fail "SSH socket was not restored before the service"
grep -Fxq broker-stop "$MOCK_INCUS_LOG" || fail "active-window stop did not pause the broker"
grep -Fxq broker-start "$MOCK_INCUS_LOG" || fail "active-window refusal did not resume the broker"
broker_start_line="$(grep -nFx broker-start "$MOCK_INCUS_LOG" | cut -d: -f1)"
restore_line="$(grep -nFx ssh-restore "$MOCK_INCUS_LOG" | cut -d: -f1)"
[ "$restore_line" -lt "$broker_start_line" ] \
  || fail "active-window refusal resumed the broker before restoring SSH"
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
broker_stop_line="$(grep -nFx broker-stop "$MOCK_INCUS_LOG" | cut -d: -f1)"
broker_start_line="$(grep -nFx broker-start "$MOCK_INCUS_LOG" | cut -d: -f1)"
if [ "$snapshot_line" -ge "$quiesce_line" ] || [ "$quiesce_line" -ge "$broker_stop_line" ] \
    || [ "$broker_stop_line" -ge "$probe_line" ] || [ "$probe_line" -ge "$stop_line" ] \
    || [ "$stop_line" -ge "$broker_start_line" ]; then
  fail "idle stop did not run quiesce -> broker pause -> probe -> stop -> broker resume"
fi

reset_case
MOCK_VSCODE_RC=9
if "$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1; then
  fail "stop accepted an unknown VS Code state"
fi
grep -Fq 'could not verify' "$TMP/out" || fail "unknown-state failure is unclear"
if grep -Fxq stop "$MOCK_INCUS_LOG"; then fail "unknown-state guard reached incus stop"; fi
grep -Fxq ssh-restore "$MOCK_INCUS_LOG" || fail "unknown-state stop did not restore SSH"
grep -Fxq broker-stop "$MOCK_INCUS_LOG" || fail "unknown-state stop did not pause the broker"
grep -Fxq broker-start "$MOCK_INCUS_LOG" || fail "unknown-state stop did not resume the broker"

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
printf 'unmanaged GitHub broker\n' \
  > "$SUBYARD_OPERATOR_HOME/.config/systemd/user/subyard-github-test-yard.service"
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1
if grep -Fxq broker-stop "$MOCK_INCUS_LOG" || grep -Fxq broker-start "$MOCK_INCUS_LOG"; then
  fail "unmanaged broker service was mutated"
fi
printf '# Managed by Subyard GitHub broker\n' \
  > "$SUBYARD_OPERATOR_HOME/.config/systemd/user/subyard-github-test-yard.service"

reset_case
export MOCK_BROKER_STATE=inactive
printf 'inactive\n' > "$MOCK_BROKER_STATE_FILE"
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1
if grep -Fxq broker-stop "$MOCK_INCUS_LOG" || grep -Fxq broker-start "$MOCK_INCUS_LOG"; then
  fail "inactive broker service was mutated"
fi

printf 'STOPPED\n' > "$MOCK_INCUS_STATE"
: > "$MOCK_INCUS_LOG"
"$ROOT/scripts/lifecycle-guard.sh" stop > "$TMP/out" 2>&1
if grep -Fxq vscode-probe "$MOCK_INCUS_LOG"; then fail "already-stopped yard ran the VS Code probe"; fi

printf 'ok: yard stop protects active VS Code Remote-SSH state\n'
