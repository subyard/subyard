#!/usr/bin/env bash
# Teardown pauses every installed profile owner service around its SSH session
# check, including after a failed first init. Only in a guest init never
# provisioned does a guest-only owner have nothing to pause.
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
  "exec "*) printf '%s\n' "${MOCK_PROVISIONED:-provisioned}" ;;
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
# A guest-only owner has nothing to pause in a guest init never provisioned.
[ "${MOCK_OWNER_GUEST_ONLY:-0}" = 1 ] && [ "${SUBYARD_GUEST_NEVER_PROVISIONED:-0}" = 1 ] && exit 0
printf 'profile %s\n' "$1" >> "$MOCK_LOG"
[ "$1" != --pause ] || exit "${MOCK_PAUSE_RC:-0}"
SH
chmod +x "$TMP/bin/incus" "$TMP/scripts/profile-services.sh"
export PATH="$TMP/bin:$PATH"

run_guard() {
  : > "$MOCK_LOG"
  (
    die() { printf '%s\n' "$*" >&2; exit 1; }
    have_incus=1 INCUS_PROJECT=subyard-y8 DEV_USER=dev
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

# A failed first init may have installed a host owner (the broker): it is paused.
export MOCK_INCUS_CONFIG="true false -" MOCK_PROVISIONED=unprovisioned MOCK_PAUSE_RC=0 MOCK_OWNER_GUEST_ONLY=0
run_guard || fail "never-provisioned teardown with an installed owner refused: $(tr '\n' ' ' < "$TMP/out")"
grep -Fxq 'profile --pause' "$MOCK_LOG" || fail "never-provisioned teardown did not pause the installed owner"
grep -Fxq fence "$MOCK_LOG" || fail "never-provisioned teardown skipped the SSH fence"
grep -Fxq session-check "$MOCK_LOG" || fail "never-provisioned teardown skipped the session check"

# A guest-only owner, which may fail in the unprovisioned guest, is not entered.
export MOCK_INCUS_CONFIG="true false -" MOCK_PROVISIONED=unprovisioned MOCK_PAUSE_RC=2 MOCK_OWNER_GUEST_ONLY=1
run_guard || fail "never-provisioned teardown with a guest-only owner refused: $(tr '\n' ' ' < "$TMP/out")"
if grep -Fq 'profile --pause' "$MOCK_LOG"; then fail "guest-only owner paused in a never-provisioned guest"; fi

# A provisioned guest, a working yard whose re-init failed, a yard from a release
# before the completion mark and an unreadable state all pause strictly.
for config in "true false -:provisioned" "true false true:unprovisioned" "true true -:unprovisioned" ":unprovisioned"; do
  export MOCK_INCUS_CONFIG="${config%%:*}" MOCK_PROVISIONED="${config#*:}" MOCK_PAUSE_RC=2 MOCK_OWNER_GUEST_ONLY=1
  if run_guard; then fail "teardown with '$config' accepted a failed owner-service pause"; fi
  grep -Fq 'could not pause profile owner services before teardown' "$TMP/out" \
    || fail "pause failure with '$config' is unclear: $(tr '\n' ' ' < "$TMP/out")"
done

printf 'ok: teardown pauses installed owner services, also after a failed first init\n'
