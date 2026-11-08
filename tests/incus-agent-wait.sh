#!/usr/bin/env bash
# The agent wait tolerates the brief STOPPED state of an Incus guest restart and
# stops waiting for an instance that stays STOPPED.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-agent-wait.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

install -d -m 0700 "$TMP/bin"
export PATH="$TMP/bin:$PATH" MOCK_DIR="$TMP"
# Each probe consumes one scripted step: "agent" answers; any other word is the
# power state that `incus list` reports after the failed probe. The last step repeats.
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
step() {
  local index
  index="$(cat "$MOCK_DIR/index")"
  mapfile -t steps < "$MOCK_DIR/steps"
  [ "$index" -lt "${#steps[@]}" ] || index=$((${#steps[@]} - 1))
  printf '%s\n' "${steps[$index]}"
}
case "$1 $2 $3 $4" in
  'exec yard --project subyard')
    [ "$*" = 'exec yard --project subyard -- true' ] || exit 90
    current="$(step)"
    [ "$current" = agent ] && exit 0
    exit 1 ;;
  'list yard --project subyard')
    [ "$*" = 'list yard --project subyard -f csv -c ns' ] || exit 90
    # A stalled Incus answers nothing until it is killed.
    [ "$(step)" != stall ] || exec sleep 60
    # A prefix-matched neighbour never stands in for the yard.
    printf 'yard-x,STOPPED\nyard,%s\n' "$(step)"
    printf '%s\n' "$(($(cat "$MOCK_DIR/index") + 1))" > "$MOCK_DIR/index" ;;
  *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK
chmod 0700 "$TMP/bin/incus"

# shellcheck source=scripts/lib/ui.sh
. "$ROOT/scripts/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$ROOT/scripts/lib/host.sh"

wait_with() { # <steps...>
  printf '%s\n' "$@" > "$TMP/steps"
  printf '0\n' > "$TMP/index"
  local rc=0 started=$SECONDS
  incus_wait_instance_agent subyard yard || rc=$?
  WAIT_ELAPSED=$((SECONDS - started))
  return "$rc"
}

export YARD_KIND=vm SUBYARD_INCUS_STOPPED_WINDOW=3 SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=30

# A guest restart reports STOPPED for less than the window; the wait continues.
wait_with RUNNING STOPPED STOPPED RUNNING agent || fail 'transient STOPPED ended the agent wait'

# A STOPPED state held for the whole window ends the wait long before its deadline.
rc=0
wait_with RUNNING STOPPED || rc=$?
[ "$rc" = 3 ] || fail "sustained STOPPED returned $rc"
[ "$WAIT_ELAPSED" -lt 10 ] || fail "sustained STOPPED waited ${WAIT_ELAPSED}s"

# A STOPPED observation interrupted by another state starts a new window.
wait_with STOPPED STOPPED RUNNING STOPPED STOPPED RUNNING agent \
  || fail 'separate short STOPPED windows were added together'

# A running guest without an agent still waits for the full deadline.
rc=0
SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=4 wait_with RUNNING || rc=$?
[ "$rc" = 1 ] || fail "unresponsive running agent returned $rc"
[ "$WAIT_ELAPSED" -ge 3 ] || fail 'unresponsive running agent ended before its deadline'

# A stalled state read is bounded by the remaining budget, not its own duration.
rc=0
SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=3 wait_with stall || rc=$?
[ "$rc" = 1 ] || fail "stalled state read returned $rc"
[ "$WAIT_ELAPSED" -le 5 ] || fail "stalled state read extended the wait to ${WAIT_ELAPSED}s"

rc=0
SUBYARD_INCUS_STOPPED_WINDOW=0 wait_with agent || rc=$?
[ "$rc" = 2 ] || fail "invalid STOPPED window returned $rc"

detail="$(incus_agent_wait_detail 3 subyard yard)"
[ "$detail" = "; instance 'yard' stopped and is not booting (its last boot log: incus info yard --project subyard --show-log)" ] \
  || fail "stopped detail: $detail"
[ -z "$(incus_agent_wait_detail 1 subyard yard)" ] || fail 'timeout detail is not empty'

printf 'ok: agent wait tolerates a guest restart and refuses an instance that stays stopped\n'
