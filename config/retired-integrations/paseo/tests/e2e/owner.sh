#!/usr/bin/env bash
# Published Paseo -> packaged removal, including a foreign unit and a stopped yard.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
[[ "${2:-}" =~ ^[0-9]{1,10}$ ]] || { printf 'expected fresh numeric token\n' >&2; exit 2; }
case "${1:-}" in prepare|resume|clean|evidence) ;; *) printf 'expected prepare, resume, clean or evidence\n' >&2; exit 2 ;; esac
cd "$ROOT"
# Reuse the disposable operator, root runtime snapshot and marker-guarded cleanup.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
# shellcheck source=dev/e2e/published-baseline.sh
. "$ROOT/dev/e2e/published-baseline.sh"
CONFIG_HOME="$OPERATOR_HOME/.config/subyard"
# shellcheck source=tests/helpers/known-release-final-check.sh
. "$ROOT/tests/helpers/known-release-final-check.sh"
YARDS=(default solo foreign)
SENTINEL=/srv/agents/paseo/worktrees/fixture/unpushed
FOREIGN_UNIT=/etc/systemd/system/paseo.service
FOREIGN_DATA=/srv/agents/paseo/data/foreign-sentinel
LOGS=(update-check.json update-check.txt update.log status-default.json status-solo.json status-foreign.json
  stopped-status.json stopped-plan.log stopped-init.log stopped-repeat-init.log stopped-proven-status.json solo-start.log
  repeat-init.log repeat-migrate.log enable-refused.log disable.log postboot.json
  final-check.json final-journal.json final-repair.log final-ready.json)

project_of() { if [ "$1" = default ]; then printf subyard; else printf 'subyard-%s' "$1"; fi; }
instance_of() { if [ "$1" = default ]; then printf yard; else printf 'yard-%s' "$1"; fi; }
guest_yard() {
  local name="$1"
  shift
  incus exec "$(instance_of "$name")" --project "$(project_of "$name")" -- "$@"
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
    for name in solo foreign; do
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
trap 'printf "paseo-retirement: line %s failed (exit %s)\n" "$LINENO" "$?" >&2' ERR

seed_settings() {
  local name port selection
  # Each baseline creates its own project/image store. The prepared alias lives
  # in project default; use the remote image supported by both published owners.
  operator_env bash -c 'sed -i "/^AGENTS=/d; /^BASE_IMAGE=/d; /^BASE_IMAGE_FALLBACK=/d; /^YARD_IMAGE=/d; /^YARD_IMAGE_FALLBACK=/d" "$1"
    printf "BASE_IMAGE=images:debian/13/cloud\nBASE_IMAGE_FALLBACK=images:debian/13/cloud\n" >> "$1"' _ "$CONFIG_HOME/config.env"
  for name in "${YARDS[@]}"; do
    case "$name" in
      default) port=2222; selection="'codex paseo'" ;;
      solo) port=2233; selection=paseo ;;
      foreign) port=2244; selection=codex ;;
    esac
    operator_env install -d -m 0700 "$CONFIG_HOME/yards/$name"
    # A retired package setting must keep loading after the update.
    operator_env bash -c 'printf "HOST_BASE=%s\nRESTRICTED_DISK_PATHS=%s\nFORWARD_SSH_AGENT=0\nSSH_PORT=%s\nCODING_TOOL_INTEGRATIONS=%s\nAGENT_paseo_COMMAND=paseo\n" \
      "$2" "$2" "$3" "$4" > "$1"; chmod 0600 "$1"' _ \
      "$CONFIG_HOME/yards/$name/config.env" "$OPERATOR_HOME/host-data-$name" "$port" "$selection"
  done
  operator_env bash -c 'sha256sum "$1"/yards/*/config.env' _ "$CONFIG_HOME" > "$STATE_ROOT/settings.before"
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

wait_active() {
  local name="$1"
  for _ in $(seq 1 90); do
    guest_yard "$name" systemctl is-active --quiet paseo.service && return
    sleep 1
  done
  die 'published Paseo did not become active'
}

seed_guests() {
  local name
  for name in "${YARDS[@]}"; do
    [ "$name" = foreign ] || wait_active "$name"
  done
  guest_yard default test -f /var/lib/subyard/paseo-ownership
  guest_yard default sh -eu -c 'install -d "$(dirname "$1")"; printf "%s\n" "$2" > "$1"' subyard "$SENTINEL" "$MARKER"
  # A retired package never adopts a service it cannot prove it installed, nor its data.
  guest_yard foreign sh -eu -c '
    [ ! -e /var/lib/subyard/paseo-ownership ]
    install -d "$(dirname "$2")"
    printf "%s\n" legacy-paseo-data > "$2"
    printf "%s\n" "[Service]" "ExecStart=/usr/bin/sleep infinity" "[Install]" "WantedBy=multi-user.target" > "$1"
    chmod 0644 "$1"
    systemctl daemon-reload
    systemctl enable --now paseo.service
  ' subyard "$FOREIGN_UNIT" "$FOREIGN_DATA"
}

assert_removed() {
  local name="$1"
  guest_yard "$name" sh -eu -c '
    ! systemctl is-active --quiet paseo.service
    for path in /etc/systemd/system/paseo.service /run/systemd/system/paseo.service \
      /usr/lib/systemd/system/paseo.service /lib/systemd/system/paseo.service /var/lib/subyard/paseo-ownership /opt/subyard/paseo \
      /usr/local/bin/paseo /usr/local/bin/paseo-check /usr/local/bin/paseo-pair \
      /usr/local/bin/paseo-rotate-identity /usr/local/bin/paseo-sync-projects; do
      [ ! -e "$path" ] && [ ! -L "$path" ]
    done
    command -v codex >/dev/null
  ' || die "yard $name kept a removable Paseo file or lost Codex"
}

yard_power() { incus list "$(instance_of "$1")" --project "$(project_of "$1")" -f csv -c s; }
retired_proof() { incus config get "$(instance_of "$1")" user.subyard.retired_integration.paseo --project "$(project_of "$1")"; }
last_boot() { incus query "/1.0/instances/$(instance_of "$1")?project=$(project_of "$1")" | jq -er .last_used_at; }

assert_status() {
  local name="$1" effective="$2"
  operator_yard -Y "$name" integration status --json > "$STATE_ROOT/status-$name.json"
  jq -e --argjson effective "$effective" '.observed == "ready" and .selection.effective == $effective' \
    "$STATE_ROOT/status-$name.json" >/dev/null || die "yard $name did not converge after retirement"
}

assert_converged() {
  local name
  for name in "${YARDS[@]}"; do assert_removed "$name"; done
  guest_yard default sh -eu -c '
    [ "$(cat "$1")" = "$2" ]
    [ "$(find /srv/agents/paseo -mindepth 1 -maxdepth 1 -printf "%f\n")" = worktrees ]
  ' subyard "$SENTINEL" "$MARKER" || die 'removal lost worktrees or retained other state'
  assert_status default '["codex"]'
  jq -e '.selection.retired == ["paseo"] and (.detail | contains("/srv/agents/paseo/worktrees"))' \
    "$STATE_ROOT/status-default.json" >/dev/null || die 'status omitted retained worktrees'
  operator_env sha256sum -c "$STATE_ROOT/settings.before" >/dev/null || die 'removal rewrote authored settings'
  for name in solo foreign; do
    guest_yard "$name" sh -eu -c '[ ! -e /srv/agents/paseo ] && [ ! -L /srv/agents/paseo ]' || die "removal retained state: $name"
  done
  assert_status solo '[]'
  assert_status foreign '["codex"]'
}

update_candidate() {
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" "$@"
}

finish_known_final_check() {
  local original="$1" current rc=0
  [ "$original" = 1 ] || return 1
  tr -d '\r' < "$STATE_ROOT/update.log" | grep -Fx \
    'yard: update: final update check: the inspected release transition has not started; next: run yard update' >/dev/null || return 1
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || return 1
  current="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" || return 1
  [[ "$current" = "releases/$CANDIDATE_VERSION-"* ]] || return 1
  current="${current#releases/}"
  operator_yard migrate --check --json > "$STATE_ROOT/final-check.json" || return 1
  operator_env cat "$CONFIG_HOME/release-transition/v2/journal.json" > "$STATE_ROOT/final-journal.json" || return 1
  # The retained old-caller log has no fresh decision JSON. Keep the exact shared
  # materialized-only allowlist; disclose and refuse any different decision set.
  jq '{outcome: (.outcome | {status,code,active,target}), decisions, blockers}' "$STATE_ROOT/final-check.json"
  known_materialized_final_check "$STATE_ROOT/final-check.json" "$STATE_ROOT/final-journal.json" \
    "$current" "${OLD_RELEASE_TARGET#releases/}" || return 1
  printf 'paseo-removal: released caller native_exit=%s; one candidate materialized-config repair\n' "$original"
  update_candidate --yes > "$STATE_ROOT/final-repair.log" 2>&1 || rc=$?
  cat "$STATE_ROOT/final-repair.log"
  printf 'paseo-removal: candidate follow-up native_exit=%s\n' "$rc"
  [ "$rc" = 0 ] || return 1
  operator_yard migrate --check --json > "$STATE_ROOT/final-ready.json" || return 1
  jq -e --arg target "$current" '.outcome.status == "ready" and .outcome.code == "ready" and
    .outcome.reachedGoal == true and .outcome.active == $target and .outcome.target == $target and
    (.blockers // []) == []' "$STATE_ROOT/final-ready.json" >/dev/null
}

prepare_acceptance() {
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
  seed_settings
  create_yards
  seed_guests
  operator_yard -Y solo stop --yes
  update_candidate --check --json > "$STATE_ROOT/update-check.json" || die 'update check failed with Paseo selected'
  jq -e '(.blockers | length) == 0' "$STATE_ROOT/update-check.json" >/dev/null || die 'Paseo produced an update blocker'
  # Frozen protocol evidence: the old updater renders the candidate consequence.
  update_candidate --check > "$STATE_ROOT/update-check.txt" 2>&1 || true
  grep -Fq 'retire integration paseo' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'old updater preview omitted the retirement'
  grep -Fq 'run: yard -Y solo init' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'update check omitted the stopped yard action'
  update_candidate --yes > "$STATE_ROOT/update.log" 2>&1 || {
    local rc=$?
    tail -c 16384 "$STATE_ROOT/update.log" >&2
    finish_known_final_check "$rc" || return "$rc"
  }
  grep -Fq 'Warning: yard default: retire integration paseo' "$STATE_ROOT/update.log" \
    || die 'unattended update omitted the retirement warning'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate was not activated'
  grep -Fq 'Warning: retired integration paseo may still be installed in yard solo; run: yard -Y solo init' "$STATE_ROOT/update.log" \
    || die 'unattended update omitted the stopped yard warning'
  [ "$(yard_power solo)" = STOPPED ] || die 'update started the stopped yard'
  assert_stopped_init_retires
  assert_converged
  operator_yard -Y default init --yes > "$STATE_ROOT/repeat-init.log" 2>&1
  operator_yard migrate --yes > "$STATE_ROOT/repeat-migrate.log" 2>&1
  ! grep -Fq 'retire integration paseo' "$STATE_ROOT/repeat-init.log" "$STATE_ROOT/repeat-migrate.log" \
    || die 'repeat reconcile planned another retirement'
  assert_converged
  if operator_yard -Y foreign integration enable paseo --yes > "$STATE_ROOT/enable-refused.log" 2>&1; then
    die 'retired integration was enabled'
  fi
  grep -Fq 'integration paseo is retired' "$STATE_ROOT/enable-refused.log" || die 'retired enable refusal is unclear'
  write_fixture_value "$BOOT_ID_STATE" "$(cat /proc/sys/kernel/random/boot_id)"
  write_fixture_value "$PHASE_STATE" retired
  PRESERVE_FIXTURE=1
  printf 'ok: Paseo removal passed; reboot pending\n'
}

assert_stopped_init_retires() {
  local booted
  operator_yard -Y solo integration status --json > "$STATE_ROOT/stopped-status.json"
  jq -e '.observed == "stopped" and (.detail | contains("run: yard -Y solo init"))' "$STATE_ROOT/stopped-status.json" >/dev/null \
    || die 'stopped status omitted the action'
  if operator_yard -Y solo init </dev/null > "$STATE_ROOT/stopped-plan.log" 2>&1; then die 'EOF accepted the temporary start'; fi
  grep -Fq 'temporarily start this stopped yard to retire integration paseo' "$STATE_ROOT/stopped-plan.log" \
    && grep -Fq 'chats, schedules, loops and uploads' "$STATE_ROOT/stopped-plan.log" \
    || die 'init plan omitted the temporary start or the retirement'
  [ "$(yard_power solo)" = STOPPED ] || die 'declined init started the yard'
  [ -z "$(retired_proof solo)" ] || die 'declined init published retirement proof'
  operator_yard -Y solo init --yes > "$STATE_ROOT/stopped-init.log" 2>&1
  [ "$(yard_power solo)" = STOPPED ] || die 'init did not restore the stopped yard'
  [ "$(retired_proof solo)" = true ] || die 'init did not publish retirement proof'
  booted="$(last_boot solo)"
  operator_yard -Y solo init --yes > "$STATE_ROOT/stopped-repeat-init.log" 2>&1
  [ "$(yard_power solo)" = STOPPED ] && [ "$(last_boot solo)" = "$booted" ] || die 'repeat init booted a proven yard'
  ! grep -Fq 'temporarily start' "$STATE_ROOT/stopped-repeat-init.log" || die 'repeat init planned another temporary start'
  operator_yard -Y solo integration status --json > "$STATE_ROOT/stopped-proven-status.json"
  jq -e '.observed == "stopped" and (.detail // "" | contains("yard -Y solo init") | not)' "$STATE_ROOT/stopped-proven-status.json" >/dev/null \
    || die 'proven yard still warns'
  operator_yard -Y solo start --yes > "$STATE_ROOT/solo-start.log" 2>&1
  ! grep -Fq 'retired integration paseo' "$STATE_ROOT/solo-start.log" || die 'start warned about a proven yard'
}

resume_acceptance() {
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
  done
  assert_converged
  operator_yard migrate --check --json > "$STATE_ROOT/postboot.json"
  jq -e '.outcome.code == "ready" and (.blockers | length) == 0' "$STATE_ROOT/postboot.json" >/dev/null \
    || die 'rebooted host is not ready'
  operator_yard -Y default integration disable paseo --yes > "$STATE_ROOT/disable.log" 2>&1
  operator_env grep -Fq "CODING_TOOL_INTEGRATIONS='codex'" "$CONFIG_HOME/yards/default/config.env" \
    || die 'retired disable did not clear the selection'
  PRESERVE_FIXTURE=1
  printf 'ok: Paseo removal survived the reboot\n'
}

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
