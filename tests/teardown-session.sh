#!/usr/bin/env bash
# Teardown pauses profile owner services around its SSH session check, except
# in a yard whose init never finished: that guest has no installed owner
# services and may lack what their handlers need.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

mkdir -p "$TMP/bin" "$TMP/scripts"
export MOCK_LOG="$TMP/calls.log"
cat > "$TMP/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1 ${2:-}" in
  "list "*) printf '%s,RUNNING\n' "$2" ;;
  "config get")
    # MOCK_INCUS_CONFIG is "managed initialized ever_initialized"; "-" is unset.
    [ -n "${MOCK_INCUS_CONFIG:-}" ] || exit 1
    read -r managed initialized ever <<<"$MOCK_INCUS_CONFIG"
    case "$4" in
      user.subyard.managed) value=$managed ;;
      user.subyard.initialized) value=$initialized ;;
      user.subyard.ever_initialized) value=$ever ;;
      *) exit 1 ;;
    esac
    [ "$value" = - ] || printf '%s\n' "$value"
    ;;
  *) exit 90 ;;
esac
SH
cat > "$TMP/scripts/profile-services.sh" <<'SH'
#!/usr/bin/env bash
printf 'profile %s\n' "$1" >> "$MOCK_LOG"
[ "$1" != --pause ] || exit "${MOCK_PAUSE_RC:-0}"
SH
chmod +x "$TMP/bin/incus" "$TMP/scripts/profile-services.sh"
export PATH="$TMP/bin:$PATH"

run_guard() {
  : > "$MOCK_LOG"
  (
    die() { printf '%s\n' "$*" >&2; exit 1; }
    have_incus=1 INCUS_PROJECT=subyard-y8
    SCRIPT_DIR="$ROOT/scripts"
    # shellcheck source=scripts/lib/teardown-session.sh
    . "$ROOT/scripts/lib/teardown-session.sh"
    SCRIPT_DIR="$TMP/scripts"
    ssh_listener_capture() { printf 'capture\n' >> "$MOCK_LOG"; }
    ssh_listener_fence() { printf 'fence\n' >> "$MOCK_LOG"; }
    ssh_listener_running() { return 1; }
    ssh_listener_restore() { :; }
    teardown_ssh_state() { printf 'session-check\n' >> "$MOCK_LOG"; printf 'idle\n'; }
    teardown_session_guard yard-y8
  ) > "$TMP/out" 2>&1
}

export MOCK_INCUS_CONFIG="true false -" MOCK_PAUSE_RC=2
run_guard || fail "never-initialized teardown refused: $(tr '\n' ' ' < "$TMP/out")"
grep -Fxq fence "$MOCK_LOG" || fail "never-initialized teardown skipped the SSH fence"
grep -Fxq session-check "$MOCK_LOG" || fail "never-initialized teardown skipped the session check"
if grep -Fq 'profile --pause' "$MOCK_LOG"; then fail "never-initialized teardown paused owner services"; fi

for config in "true false true" "true true -" ""; do
  export MOCK_INCUS_CONFIG="$config" MOCK_PAUSE_RC=2
  if run_guard; then fail "teardown with '$config' accepted a failed owner-service pause"; fi
  grep -Fq 'could not pause profile owner services before teardown' "$TMP/out" \
    || fail "pause failure with '$config' is unclear: $(tr '\n' ' ' < "$TMP/out")"
done

export MOCK_INCUS_CONFIG="true false true" MOCK_PAUSE_RC=0
run_guard || fail "re-initialized teardown refused: $(tr '\n' ' ' < "$TMP/out")"
grep -Fxq 'profile --pause' "$MOCK_LOG" || fail "re-initialized teardown did not pause owner services"

printf 'ok: teardown pauses owner services except in a yard whose init never finished\n'
