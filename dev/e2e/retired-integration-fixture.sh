#!/usr/bin/env bash
# Shared retained-lease helpers for retired-integration payloads of
# dev/e2e/disabled-components-release.sh. The payload sets FIXTURE_LABEL, YARDS
# (default first) and LOGS, defines prepare_acceptance and resume_acceptance,
# then calls run_retirement_payload.
# shellcheck disable=SC2034 # consumed by the sourcing payload
[[ "${2:-}" =~ ^[0-9]{1,10}$ ]] || { printf 'expected fresh numeric token\n' >&2; exit 2; }
case "${1:-}" in prepare|resume|clean|evidence) ;; *) printf 'expected prepare, resume, clean or evidence\n' >&2; exit 2 ;; esac
cd "$ROOT"
# Reuse the disposable operator, root runtime snapshot and marker-guarded cleanup.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
# shellcheck source=dev/e2e/published-baseline.sh
. "$ROOT/dev/e2e/published-baseline.sh"
CONFIG_HOME="$OPERATOR_HOME/.config/subyard"

report_failure() { # message, captured files...
  local message="$1" file
  shift
  printf '%s: %s\n' "${FIXTURE_LABEL:-retirement}" "$message" >&2
  for file; do
    [ -f "$file" ] || continue
    printf '\n== %s ==\n' "${file##*/}" >&2
    case "$file" in *.json) cat "$file" >&2 ;; *) tail -c 16384 "$file" >&2 ;; esac
  done
  return 1
}

check_json() { # captured file, message, jq arguments...
  local file="$1" message="$2"
  shift 2
  jq -e "$@" "$file" >/dev/null || report_failure "$message" "$file"
}

run_checked() { # log name, operation label, command...
  local file="$STATE_ROOT/$1" label="$2" rc=0
  shift 2
  case "$file" in
    *.json) "$@" > "$file" || rc=$? ;;
    *) "$@" > "$file" 2>&1 || rc=$? ;;
  esac
  if [ "$rc" != 0 ]; then
    report_failure "$label native_exit=$rc" "$file" || return "$rc"
  fi
}

project_of() { if [ "$1" = default ]; then printf subyard; else printf 'subyard-%s' "$1"; fi; }
instance_of() { if [ "$1" = default ]; then printf yard; else printf 'yard-%s' "$1"; fi; }
guest_yard() {
  local name="$1"
  shift
  incus exec "$(instance_of "$name")" --project "$(project_of "$name")" -- "$@"
}
yard_power() { incus list "$(instance_of "$1")" --project "$(project_of "$1")" -f csv -c s; }

update_candidate() {
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" "$@"
}

fixture_cleanup() {
  local rc=$? failed=0 name
  if [ "$rc" = 0 ] && [ "$PRESERVE_FIXTURE" = 1 ]; then cleanup; fi
  trap - EXIT INT TERM ERR
  set +e
  if [ "$CLEANUP_ARMED" = 1 ]; then
    assert_state_root
    if [ "$rc" != 0 ]; then
      install -d -m 0755 "$ROOT/.build/disabled-components-release"
      sudo -n tar --ignore-failed-read -C "$STATE_ROOT" -czf \
        "$ROOT/.build/disabled-components-release/failure.tar.gz" -- "${LOGS[@]}" 2>/dev/null || failed=1
    fi
    for name in "${YARDS[@]:1}"; do
      if [ -f "$STATE_ROOT/.$name-armed" ]; then
        (PROJECT="$(project_of "$name")" INSTANCE="$(instance_of "$name")"; delete_owned_project) || failed=1
      fi
    done
  fi
  [ "$failed" = 0 ] || rc=3
  (exit "$rc")
  cleanup
}

trap fixture_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'printf "%s: line %s failed (exit %s)\n" "$FIXTURE_LABEL" "$LINENO" "$?" >&2' ERR

# Install the published anchor and stage the frozen candidate's release assets.
prepare_candidate_fixture() {
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  release_candidate_prepare "$ROOT" >/dev/null
  CANDIDATE_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  ensure_platform
  prepare_fixture
  install -d -m 0755 "$RELEASE_ROOT"
  cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
  chmod -R a+rX "$RELEASE_ROOT"
  install_baseline
  OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
}

create_yards() {
  local name project
  for name in "${YARDS[@]}"; do
    project="$(project_of "$name")"
    ! incus project show "$project" >/dev/null 2>&1 || die 'fixture project already exists'
    if [ "$name" = default ]; then printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
    else printf '%s\n' "$MARKER" > "$STATE_ROOT/.$name-armed"; fi
    # Let the published release create its native project. Pre-creating it makes
    # newer predecessors require adoption before ordinary initialization.
    operator_yard -Y "$name" init --yes
    incus project set "$project" user.p0-power-systemd "$MARKER"
    operator_yard -Y "$name" start --yes
  done
}

# A fixture-only native stop reaches the externally stopped state without passing
# the release gate. Bind both physical stop and stopped intent to our project/UUID.
stop_owned_solo() { # captured UUID
  (
    local PROJECT=subyard-solo INSTANCE=yard-solo
    assert_state_root
    [ -f "$STATE_ROOT/.solo-armed" ] && [ ! -L "$STATE_ROOT/.solo-armed" ] \
      && [ "$(cat "$STATE_ROOT/.solo-armed")" = "$MARKER" ] || die 'refusing unarmed solo stop'
    assert_project_owned
    [ "$(incus config get "$INSTANCE" user.subyard.managed --project "$PROJECT")" = true ] \
      || die 'refusing foreign solo instance'
    [ -n "$1" ] && [ "$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")" = "$1" ] \
      || die 'solo instance identity changed before native stop'
    incus stop "$INSTANCE" --project "$PROJECT" || exit $?
    [ "$(yard_power solo)" = STOPPED ] || die 'native stop did not stop solo'
    [ "$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")" = "$1" ] \
      || die 'solo instance identity changed after native stop'
    # Init restores desired power, not just observed power. Preserve stopped intent too.
    incus config set "$INSTANCE" user.subyard.desired_power stopped --project "$PROJECT"
  )
}

# Wait until every fixture yard returns running after the controller's reboot.
await_rebooted_yards() {
  local name
  assert_state_root
  CLEANUP_ARMED=1
  assert_fixture_phase retired
  [ "$(cat /proc/sys/kernel/random/boot_id)" != "$(read_fixture_value "$BOOT_ID_STATE")" ] || die 'reboot was not observed'
  CANDIDATE_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  for name in "${YARDS[@]}"; do
    for _ in $(seq 1 120); do
      [ "$(yard_power "$name")" = RUNNING ] && break
      sleep 1
    done
    [ "$(yard_power "$name")" = RUNNING ] || die "yard $name did not return running after reboot"
  done
}

record_reboot_pending() {
  write_fixture_value "$BOOT_ID_STATE" "$(cat /proc/sys/kernel/random/boot_id)"
  write_fixture_value "$PHASE_STATE" retired
  PRESERVE_FIXTURE=1
}

run_retirement_payload() {
  local command
  for command in sudo systemctl timeout cmp curl jq sha256sum python3; do
    command -v "$command" >/dev/null || die "$command is required"
  done
  sudo -n true || die 'passwordless sudo is required on disposable worker'
  case "$MODE" in
    prepare) prepare_acceptance ;;
    resume) resume_acceptance ;;
    evidence)
      assert_state_root
      sudo -n tar --ignore-failed-read -C "$STATE_ROOT" -czf - -- "${LOGS[@]}"
      PRESERVE_FIXTURE=1
      ;;
    clean) if [ -e "$STATE_ROOT" ]; then assert_state_root; CLEANUP_ARMED=1; fi ;;
  esac
}
