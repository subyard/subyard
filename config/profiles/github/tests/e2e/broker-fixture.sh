# shellcheck shell=bash
# Source-only broker lifecycle operations; state and traps belong to the caller.
assert_default_boot_registration() {
  local link="$SUBYARD_OPERATOR_HOME/.config/systemd/user/default.target.wants/$UNIT" wants
  [ -L "$link" ] && [ "$(readlink -- "$link")" = "$UNIT_FILE" ] \
    || die 'broker boot link does not select its owned unit'
  [ "$(systemctl_user show "$UNIT" --property=FragmentPath --value)" = "$UNIT_FILE" ] \
    || die 'broker loaded a different unit fragment'
  wants="$(systemctl_user show default.target --property=Wants --value)"
  case " $wants " in *" $UNIT "*) ;; *) die 'broker boot dependency is not loaded' ;; esac
}

verify_boot_registration_recovery() {
  local link="$SUBYARD_OPERATOR_HOME/.config/systemd/user/default.target.wants/$UNIT"
  local phase before_pid before_payloads before_connection wants
  before_connection="$(sha256sum "$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json")"
  before_payloads="$(stat -c '%i:%Y:%s' "$UNIT_FILE" "$SUBYARD_HOME/github-broker/default.json" \
    "$SUBYARD_HOME/github-broker/default-engine")"
  [ "$#" -gt 0 ] || set -- active inactive
  for phase in "$@"; do
    assert_default_boot_registration
    [ "$phase" != inactive ] || systemctl_user stop "$UNIT"
    before_pid="$(systemctl_user show "$UNIT" -p MainPID --value)"
    # Reproduce a persistent link arriving after the target graph was loaded.
    # The link and its unit are exact marker-owned fixture resources.
    rm -- "$link"
    systemctl_user daemon-reload
    ln -s "$UNIT_FILE" "$link"
    systemctl_user is-enabled --quiet "$UNIT" || die 'drift fixture is not persistently enabled'
    wants="$(systemctl_user show default.target --property=Wants --value)"
    case " $wants " in *" $UNIT "*) die 'drift fixture unexpectedly loaded the boot dependency' ;; esac
    yard profile status github --json | jq -e '.enabled and .configured and (.ready == false) and .runtime == "pending"' >/dev/null \
      || die 'read-only profile status missed broker registration drift'
    yard profile status github | grep -Fxq 'diagnostic: boot-dependency-unloaded' \
      || die 'profile status did not explain unloaded boot registration'
    # Release admission detects the same drift; repair through its approved
    # activation boundary rather than bypassing that guard with scoped enable.
    yard migrate --yes
    wait_active
    wait_status '{"configured":true}'
    assert_default_boot_registration
    [ "$(stat -c '%i:%Y:%s' "$UNIT_FILE" "$SUBYARD_HOME/github-broker/default.json" \
      "$SUBYARD_HOME/github-broker/default-engine")" = "$before_payloads" ] \
      || die 'registration repair reinstalled healthy broker payloads'
    if [ "$phase" = active ]; then
      [ "$(systemctl_user show "$UNIT" -p MainPID --value)" = "$before_pid" ] \
        || die 'registration repair restarted a healthy broker'
    fi
    before_pid="$(systemctl_user show "$UNIT" -p MainPID --value)"
    yard migrate --yes
    [ "$(systemctl_user show "$UNIT" -p MainPID --value)" = "$before_pid" ] \
      || die 'repeated registration repair restarted the broker'
  done
  [ "$(sha256sum "$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json")" = "$before_connection" ] \
    || die 'registration repair changed the App connection'
  ok 'enabled broker registration recovered active and inactive states without reinstalling payloads'
}

disable_default_broker() {
  local connection="$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json" connection_hash=''
  if [ -f "$connection" ]; then connection_hash="$(sha256sum "$connection" | awk '{print $1}')"; fi
  yard profile disable github --yes || return $?
  [ ! -e "$UNIT_FILE" ] && [ ! -L "$UNIT_FILE" ] || die 'disable left owner broker service'
  assert_removed
  yard profile status github --json | jq -e '.enabled == false and .configured == true and .runtime == "removed" and .ready == false' >/dev/null \
    || die 'disabled profile status is incorrect'
  if [ -n "$connection_hash" ]; then
    [ "$(sha256sum "$connection" | awk '{print $1}')" = "$connection_hash" ] || die 'disable changed the shared connection'
  fi
}

verify_scoped_broker_lifecycle() {
  local connection_hash="${1:?expected connection hash required}" opaque="${2:?opaque path required}"
  local opaque_before="${3:?expected opaque hash required}" hermes_pid profiles_before
  hermes_pid="$(systemctl_user show "$HERMES_UNIT" -p MainPID --value)"
  profiles_before="$(setting ENVIRONMENT_PROFILES)"
  guest mv /home/dev/.gitconfig /home/dev/.gitconfig-scoped-fixture
  yard profile enable github --yes
  yard profile enable github --yes
  guest test ! -e /home/dev/.gitconfig || die 'scoped enable reconciled unrelated git identity drift'
  guest mv /home/dev/.gitconfig-scoped-fixture /home/dev/.gitconfig
  yard profile status github --json | jq -e '.enabled and .configured and .ready and .runtime == "ready"' >/dev/null \
    || die 'scoped enable status is incorrect'
  wait_status '{"configured":true}'
  assert_wiring
  yard profile disable github --yes
  [ "$(setting ENVIRONMENT_PROFILES)" = "$profiles_before" ] || die 'scoped disable changed neighboring selections'
  [ "$(systemctl_user show "$HERMES_UNIT" -p MainPID --value)" = "$hermes_pid" ] || die 'scoped operations restarted another yard broker'
  [ "$(hermes_status || true)" = '{"configured":true}' ] || die 'scoped disable disconnected the other yard'
  [ "$(sha256sum "$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json" | awk '{print $1}')" = "$connection_hash" ] \
    || die 'scoped operations changed the shared connection'
  yard stop --yes
  if yard profile enable github --yes >"$STATE/stopped-profile.log" 2>&1; then die 'scoped enable accepted a stopped yard'; fi
  [ "$(setting ENVIRONMENT_PROFILES)" = "$profiles_before" ] || die 'stopped enable wrote the profile selection'
  yard profile setup github --yes
  yard profile status github --json | jq -e '.configured and .runtime == "unavailable" and .ready == false' >/dev/null \
    || die 'stopped profile status is incorrect'
  yard start --yes
  yard profile enable github --yes
  wait_status '{"configured":true}'
  [ "$(hermes_dev sha256sum "$opaque" | awk '{print $1}')" = "$opaque_before" ] \
    || die 'scoped operations changed Hermes opaque state'
}

cleanup_default_broker() {
  disable_default_broker
  # shellcheck disable=SC2034
  DEV_UID="$(guest id -u dev)"
  # shellcheck disable=SC2034
  DEV_GID="$(guest id -g dev)"
}

  broker_controller_cleanup() {
    rc=${1:-$?}
    local original_rc="$rc" existing_directory cleanup_started=$SECONDS fixture_rc release_rc=0
    trap - EXIT INT TERM
    set +e
    phase_end "$original_rc"
    # shellcheck disable=SC2034
    E2E_ACTIVE_PHASE=''
    phase_marker cleanup/release start 0 0
    # shellcheck disable=SC2154
    if [ "$prepared" = 1 ] && [ -n "${bundle:-}" ] && [ -n "${bundle_hash:-}" ]; then
      existing_directory="${GUEST_DIRS[1]:-}"
      if [ -n "$existing_directory" ]; then
        cleanup_guest 1 quiet || cleanup_failed=1
      fi
      if [ "$cleanup_failed" = 0 ]; then
        : > "$LOCAL_TEMP/fixture-cleanup.log"
        chmod 0600 "$LOCAL_TEMP/fixture-cleanup.log"
        fixture_rc=0
        run_guest 1 "$bundle" "$bundle_hash" bash config/profiles/github/tests/e2e/acceptance.sh --cleanup \
          > "$LOCAL_TEMP/fixture-cleanup.log" 2>&1 || fixture_rc=$?
        printf 'github_e2e_fixture_cleanup exit_code=%s\n' "$fixture_rc" >&2
        [ "$fixture_rc" = 0 ] || cleanup_failed=1
        cleanup_guest 1 quiet || cleanup_failed=1
      fi
    fi
    for cleanup_vm in "${!GUEST_DIRS[@]}"; do
      cleanup_guest "$cleanup_vm" quiet || cleanup_failed=1
    done
    if [ -n "${LEASE_KEEPER_PID:-}" ]; then
      kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      LEASE_KEEPER_PID=''
    fi
    release_lease || release_rc=$?
    printf 'github_e2e_release exit_code=%s\n' "$release_rc" >&2
    [ "$release_rc" = 0 ] || cleanup_failed=1
    case "${LOCAL_TEMP:-}" in
      /tmp/subyard-github-e2e.*|"${TMPDIR:-/tmp}"/subyard-github-e2e.*)
        find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_failed=1 ;;
    esac
    [ "$cleanup_failed" = 0 ] || [ "$rc" != 0 ] || rc=3
    phase_marker cleanup/release end "$((SECONDS - cleanup_started))" "$((cleanup_failed * 3))"
    printf 'github_e2e_controller_result original_exit_code=%s cleanup_exit_code=%s final_exit_code=%s\n' \
      "$original_rc" "$((cleanup_failed * 3))" "$rc" >&2
    exit "$rc"
  }
broker_guest_cleanup() {
  local rc=${1:-$?} state_cleaned=0 cleanup_rc=0
  local original_rc="$rc"
  trap - EXIT INT TERM
  set +e
  printf 'github_e2e_guest_exit mode=%s original_exit_code=%s\n' "$MODE" "$original_rc" >&2
  if [ "$rc" -ne 0 ] && [ -n "$UNIT_FILE" ] && [ -f "$UNIT_FILE" ]; then
    systemctl_user show "$UNIT" -p ActiveState -p Result -p ExecMainStatus >&2
    journalctl --user -u "$UNIT" --no-pager -n 8 >&2
  fi
  [ -z "$HERMES_PROJECT" ] || cleanup_hermes || cleanup_rc=3
  if [ -n "$PROJECT" ] && incus project show "$PROJECT" >/dev/null 2>&1; then
    project_marker="$(incus project get "$PROJECT" user.subyard.e2e 2>/dev/null || true)"
    instance_marker="$(incus config get "$INSTANCE" user.subyard.e2e --project "$PROJECT" 2>/dev/null || true)"
    if [ "$project_marker" = "$MARKER" ] && [ "$instance_marker" = "$MARKER" ] \
      || { [ "$PROJECT_CLAIMED" = 1 ] && [ -z "$project_marker" ] && [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; }; then
      yard teardown --yes >/dev/null 2>&1 || cleanup_rc=3
    else
      printf 'github-broker-e2e: refusing to remove unmarked project %s\n' "$PROJECT" >&2
      cleanup_rc=3
    fi
  fi
  cleanup_unit || cleanup_rc=3
  if [ "$cleanup_rc" = 0 ] && [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-github-broker.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; then
    if [ "$(incus storage get default source 2>/dev/null || true)" = "$STATE/storage" ]; then
      # A retained Incus pool from an older fixture must outlive its test state.
      for child in "$STATE"/*; do
        [ "$child" != "$STATE/storage" ] || continue
        [ -e "$child" ] || [ -L "$child" ] || continue
        sudo -n find "$child" -depth -delete || cleanup_rc=3
      done
    else
      sudo -n find "$STATE" -depth -delete || cleanup_rc=3
    fi
    [ "$cleanup_rc" != 0 ] || state_cleaned=1
  fi
  if { [ "$MODE" = --verify-reboot ] || [ "$MODE" = --cleanup ]; } && [ "$cleanup_rc" = 0 ] \
    && { [ "$state_cleaned" = 1 ] || { [ "$MODE" = --cleanup ] && [ "$rc" = 0 ]; }; } \
    && [ -f "$CHECKPOINT" ] && [ "$(stat -c '%a' "$CHECKPOINT")" = 600 ] \
    && [ "$(jq -r '.STATE' "$CHECKPOINT" 2>/dev/null || true)" = "$STATE" ]; then
    rm -f -- "$CHECKPOINT" || cleanup_rc=3
  fi
  [ "$original_rc" != 0 ] || rc="$cleanup_rc"
  printf 'github_e2e_guest_result mode=%s original_exit_code=%s cleanup_exit_code=%s final_exit_code=%s\n' "$MODE" "$original_rc" "$cleanup_rc" "$rc" >&2
  exit "$rc"
}
