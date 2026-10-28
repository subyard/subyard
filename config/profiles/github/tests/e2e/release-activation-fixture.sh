# shellcheck shell=bash
# Source-only release activation operations; the caller owns traps and state.
stage() { printf 'release_activation_stage=%s elapsed_seconds=%s\n' "$1" "$SECONDS"; }
release_activation_initialize_readiness() {
  READY_STATE=not-started READY_EXIT=unknown READY_STARTED=$SECONDS
}
ready() {
  READY_STATE=running READY_EXIT=unknown READY_STARTED=$SECONDS
  printf 'release_activation_ready state=start elapsed_seconds=%s\n' "$SECONDS" || :
  if yard migrate --check --json > "$STATE/readiness.json"; then
    READY_EXIT=0
  else
    READY_EXIT=$?
  fi
  READY_STATE=returned
  printf 'release_activation_ready state=returned elapsed_seconds=%s native_exit=%s operation_elapsed_seconds=%s\n' \
    "$SECONDS" "$READY_EXIT" "$((SECONDS - READY_STARTED))" || :
  [ "$READY_EXIT" = 0 ] || return "$READY_EXIT"
  jq -e '.outcome.status == "ready"' "$STATE/readiness.json" >/dev/null || return $?
  jq -c '.outcome | {status,code,warnings}' "$STATE/readiness.json" || return $?
  READY_STATE=complete
}
release_activation_cleanup() {
  local original=$? failed=0 yard project marker safe_units=1 unit path mutated_units=0
  local cleanup_loss="${NATIVE_LOSS:-}"
  trap - EXIT INT TERM
  set +e
  if [ "$original" != 0 ]; then
    # Fixed in-memory facts only; never repeat the command or print its private JSON.
    printf 'release_activation_ready_failure elapsed_seconds=%s state=%s native_exit=%s trigger_exit=%s\n' \
      "$SECONDS" "${READY_STATE:-not-started}" "${READY_EXIT:-unknown}" "$original" >&2 || :
  fi
  # An interrupted update is a fixture-owned process group, never the user manager.
  if [ -f "$STATE/updater.json" ]; then
    python3 - "$STATE/updater.json" <<'PY'
import json, os, signal, sys
value=json.load(open(sys.argv[1]))
for group in value.get('groups', [])+[value]:
    try:
        pid=group['pid']
        if os.getpgid(pid)==pid and open('/proc/'+str(pid)+'/stat').read().split()[21]==group['start']:
            os.killpg(pid, signal.SIGKILL)
    except (ProcessLookupError, FileNotFoundError):
        pass
PY
    [ "$?" = 0 ] || failed=1
  fi
  cleanup_image_probe || failed=1
  if [ "$UNITS_AUTHORIZED" = 1 ]; then
    for unit in "$UNIT" "$NAMED_UNIT" subyard-keys-sync.service subyard-keys-sync.timer; do
      path="$UNIT_DIR/$unit"
      [ -e "$path" ] || [ -L "$path" ] || continue
      unit_owned "$path" "$unit" || { safe_units=0; failed=1; }
    done
  fi
  for yard in "${CREATED_YARDS[@]}"; do
    [ "$safe_units" = 1 ] || continue
    project="$PROJECT"; [ "$yard" = default ] || project="$NAMED_PROJECT"
    if incus project show "$project" >/dev/null 2>&1; then
      marker="$(incus project get "$project" user.subyard.activation-fixture 2>/dev/null)"
      if [ "$marker" != "$STATE" ]; then
        printf 'release-activation: cleanup refused unmarked project %s\n' "$project" >&2
        failed=1
        continue
      fi
      if [ -n "$cleanup_loss" ] && [ "$cleanup_loss" != owner ] && [ -f "$STATE/native-loss/after-cut.json" ]; then
        printf 'release_activation_pending_fixture_cleanup=allocation-release yard=%s\n' "$yard"
      else
        fresh timeout --signal=TERM --kill-after=10 75 "$YARD_BIN" -Y "$yard" teardown --yes \
          > "$STATE/cleanup-$yard.log" 2>&1 || failed=1
      fi
    fi
  done
  for unit in "$UNIT" "$NAMED_UNIT" subyard-keys-sync.timer subyard-keys-sync.service; do
    [ "$UNITS_AUTHORIZED" = 1 ] || continue
    path="$UNIT_DIR/$unit"
    if [ -f "$path" ] && [ ! -L "$path" ]; then
      unit_owned "$path" "$unit" || { failed=1; continue; }
      mutated_units=1
      systemctl_user stop "$unit" || failed=1
      systemctl_user disable "$unit" >/dev/null 2>&1 || failed=1
      rm -- "$path" || failed=1
    fi
  done
  [ "$mutated_units" = 0 ] || systemctl_user daemon-reload || failed=1
  printf 'release_activation_terminal original_exit=%s cleanup_exit=%s duration_seconds=%s\n' \
    "$original" "$failed" "$(( $(date +%s) - STARTED ))"
  # Preserve full native logs before the standard runner deletes its source/VM.
  for log in "$STATE"/*.log; do
    [ -f "$log" ] || continue
    printf 'release_activation_log=%s\n' "${log##*/}"
    cat "$log"
  done
  if [ "$failed" = 0 ] && { [ -z "$cleanup_loss" ] || [ "$cleanup_loss" = owner ] || [ ! -f "$STATE/native-loss/after-cut.json" ]; }; then
    [[ "$STATE" = /tmp/subyard-release-activation.* ]] && [ ! -L "$STATE" ] \
      && [ "$(cat "$STATE/.marker")" = "$MARKER" ] || { [ "$original" != 0 ] && exit "$original"; exit 3; }
    find "$STATE" -depth -delete || { [ "$original" != 0 ] && exit "$original"; exit 3; }
  fi
  [ "$failed" = 0 ] || [ "$original" != 0 ] || exit 3
  exit "$original"
}
