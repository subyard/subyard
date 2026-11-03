#!/usr/bin/env bash
# Real published Paseo -> packaged retirement. The upgrade scenario crosses the migration epochs
# from the oldest supported release, with a foreign unit and an init-retired stopped yard, and its
# rollback is refused. The rollback scenario starts from a release that shares the candidate's
# ledger, so it covers the warning without a source migration, a rollback and a forward update.
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
SCENARIO="${PASEO_SCENARIO:-upgrade}"
case "$SCENARIO" in
  upgrade) YARDS=(default solo foreign) ;;
  rollback)
    YARDS=(default late)
    BASELINE_VERSION=0.25.4
    BASELINE_INSTALLER_SHA256=35ec3afdefe17c3d6fca43e3c2e05dae87529ff46a610079efad795ba6de8e2d
    BASELINE_AMD64_SHA256=d0e9970ce84d8cb177f962797582bf507a2c864b0b9b907f9ff6c23f460c4bd2
    BASELINE_ARM64_SHA256=62e6cfdb4b6fc9b1ffa0ca94d4a4e9d5edb1896ed147d0f77973ab4efa892de5
    ;;
  *) die 'unknown retirement scenario' ;;
esac
SENTINEL=/srv/agents/paseo/worktrees/fixture/unpushed
FOREIGN_UNIT=/etc/systemd/system/paseo.service
FOREIGN_DATA=/srv/agents/paseo/data/foreign-sentinel
LOGS=(update-check.json update-check.txt update.log status-default.json status-solo.json status-foreign.json
  stopped-status.json stopped-plan.log stopped-init.log stopped-repeat-init.log stopped-proven-status.json solo-start.log
  late-status.json late-check.json late-update.log late-start.log late-init.log
  repeat-init.log repeat-migrate.log foreign-init.log enable-refused.log rollback-refused.log rollback.log forward.log disable.log postboot.json)

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
    for name in solo foreign late; do
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
  operator_env bash -c 'sed -i "/^AGENTS=/d" "$1"' _ "$CONFIG_HOME/config.env"
  for name in "${YARDS[@]}"; do
    case "$name" in
      default) port=2222; selection="'codex paseo'" ;;
      solo) port=2233; selection=paseo ;;
      foreign) port=2244; selection=codex ;;
      late) port=2255; selection=paseo ;;
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
    incus project create "$project" -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
    operator_yard -Y "$name" init --yes
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
  [ "$SCENARIO" = upgrade ] || return 0
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
  guest_yard foreign sha256sum "$FOREIGN_UNIT" "$FOREIGN_DATA" > "$STATE_ROOT/foreign.before"
}

assert_removed() {
  local name="$1"
  guest_yard "$name" sh -eu -c '
    ! systemctl is-active --quiet paseo.service
    for path in /etc/systemd/system/paseo.service /var/lib/subyard/paseo-ownership /opt/subyard/paseo \
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

assert_foreign_kept() {
  [ "$(guest_yard foreign sha256sum "$FOREIGN_UNIT" "$FOREIGN_DATA")" = "$(cat "$STATE_ROOT/foreign.before")" ] \
    || die 'retirement changed a foreign unit or its data'
  guest_yard foreign sh -eu -c '
    systemctl is-active --quiet paseo.service
    systemctl is-enabled --quiet paseo.service
    [ ! -e /var/lib/subyard/paseo-ownership ]
  ' || die 'retirement stopped or claimed a foreign unit'
}

assert_status() {
  local name="$1" effective="$2"
  operator_yard -Y "$name" integration status --json > "$STATE_ROOT/status-$name.json"
  jq -e --argjson effective "$effective" '.observed == "ready" and .selection.effective == $effective' \
    "$STATE_ROOT/status-$name.json" >/dev/null || die "yard $name did not converge after retirement"
}

assert_converged() {
  assert_removed default
  # A worktree keeps the whole directory; without one the receipt-proven directory goes.
  guest_yard default sh -eu -c '[ "$(cat "$1")" = "$2" ]; test -f /srv/agents/paseo/config.json' subyard "$SENTINEL" "$MARKER" \
    || die 'retirement lost a Paseo worktree or its directory'
  assert_status default '["codex"]'
  jq -e '.selection.retired == ["paseo"] and (.detail | contains("chats, schedules, loops and uploads") and contains("rm -rf -- /srv/agents/paseo"))' \
    "$STATE_ROOT/status-default.json" >/dev/null || die 'status omitted the ignored selection or kept directory warning'
  operator_env sha256sum -c "$STATE_ROOT/settings.before" >/dev/null || die 'retirement rewrote authored settings'
  [ "$SCENARIO" = upgrade ] || return 0
  assert_removed solo
  assert_foreign_kept
  guest_yard solo sh -eu -c '[ ! -e /srv/agents/paseo ] && [ ! -L /srv/agents/paseo ]' || die 'retirement kept a directory without worktrees'
  assert_status solo '[]'
  assert_status foreign '["codex"]'
  jq -e '.detail | contains("paseo.service") and contains("incus exec")' "$STATE_ROOT/status-foreign.json" >/dev/null \
    || die 'status omitted the manual removal command'
}

update_candidate() {
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" "$@"
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
  seed_settings
  create_yards
  seed_guests
  if [ "$SCENARIO" = upgrade ]; then
    operator_yard -Y solo stop --yes
  else
    operator_yard -Y late stop --yes
    # Known limit: the warning needs a retired selection, so an enabled unit in a yard
    # whose settings no longer select the package stays silent until a running reconcile.
    late_settings="$CONFIG_HOME/yards/late/config.env"
    operator_env cp -p "$late_settings" "$OPERATOR_HOME/late-settings.selected"
    operator_env sed -i 's/^CODING_TOOL_INTEGRATIONS=.*/CODING_TOOL_INTEGRATIONS=/' "$late_settings"
  fi
  update_candidate --check --json > "$STATE_ROOT/update-check.json" || die 'update check failed with Paseo selected'
  jq -e '(.blockers | length) == 0' "$STATE_ROOT/update-check.json" >/dev/null || die 'Paseo produced an update blocker'
  # Frozen protocol evidence: the old updater renders the candidate consequence.
  update_candidate --check > "$STATE_ROOT/update-check.txt" 2>&1 || true
  grep -Fq 'retire integration paseo' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'old updater preview omitted the retirement'
  if [ "$SCENARIO" = upgrade ]; then
    grep -Fq 'run: yard -Y solo init' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
      || die 'update check omitted the stopped yard action'
  else
    ! grep -Fq 'yard -Y late init' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
      || die 'update check warned without a retired selection'
  fi
  update_candidate --yes > "$STATE_ROOT/update.log" 2>&1
  grep -Fq 'Warning: yard default: retire integration paseo' "$STATE_ROOT/update.log" \
    || die 'unattended update omitted the retirement warning'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate was not activated'
  if [ "$SCENARIO" = upgrade ]; then
    grep -Fq 'Warning: retired integration paseo may still be installed in yard solo; run: yard -Y solo init' "$STATE_ROOT/update.log" \
      || die 'unattended update omitted the stopped yard warning'
    [ "$(yard_power solo)" = STOPPED ] || die 'update started the stopped yard'
    assert_stopped_init_retires
  else
    ! grep -Fq 'yard -Y late init' "$STATE_ROOT/update.log" || die 'update warned without a retired selection'
    [ "$(yard_power late)" = STOPPED ] || die 'update started the stopped yard without a selection'
    assert_update_warns_without_source_migration
    assert_start_warns_until_init
  fi
  assert_converged
  operator_yard -Y default init --yes > "$STATE_ROOT/repeat-init.log" 2>&1
  operator_yard migrate --yes > "$STATE_ROOT/repeat-migrate.log" 2>&1
  ! grep -Fq 'retire integration paseo' "$STATE_ROOT/repeat-init.log" "$STATE_ROOT/repeat-migrate.log" \
    || die 'repeat reconcile planned another retirement'
  assert_converged
  if [ "$SCENARIO" = upgrade ]; then
    operator_yard -Y foreign init --yes > "$STATE_ROOT/foreign-init.log" 2>&1
    grep -Fq 'paseo.service' "$STATE_ROOT/foreign-init.log" || die 'init omitted the foreign unit notice'
    assert_foreign_kept
    if operator_yard -Y foreign integration enable paseo --yes > "$STATE_ROOT/enable-refused.log" 2>&1; then
      die 'retired integration was enabled'
    fi
    grep -Fq 'integration paseo is retired' "$STATE_ROOT/enable-refused.log" || die 'retired enable refusal is unclear'
  else
    # Reprovisioning needs more than the controller's 600 seconds for the resume phase.
    assert_rollback_and_forward
  fi
  write_fixture_value "$BOOT_ID_STATE" "$(cat /proc/sys/kernel/random/boot_id)"
  write_fixture_value "$PHASE_STATE" retired
  PRESERVE_FIXTURE=1
  printf 'ok: %s scenario of the Paseo retirement passed; reboot pending\n' "$SCENARIO"
}

# Init alone retires a stopped yard, returns it to stopped and publishes the proof
# that lets a repeat init skip the boot.
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

# With nothing left to migrate, only the activation observation can still warn.
assert_update_warns_without_source_migration() {
  local warning='retired integration paseo may still be installed in yard late; run: yard -Y late init'
  operator_env cp -p "$OPERATOR_HOME/late-settings.selected" "$late_settings"
  update_candidate --check --json > "$STATE_ROOT/late-check.json" 2>&1 || die 'fresh update check failed'
  jq -e --arg warning "$warning" '(.blockers | length) == 0 and (.outcome.warnings | index($warning) != null)' \
    "$STATE_ROOT/late-check.json" >/dev/null || die 'fresh update check omitted the stopped yard warning'
  update_candidate --yes > "$STATE_ROOT/late-update.log" 2>&1 || die 'fresh update failed'
  grep -Fq "Warning: $warning" "$STATE_ROOT/late-update.log" || die 'fresh unattended update omitted the stopped yard warning'
  [ "$(yard_power late)" = STOPPED ] || die 'fresh update started the stopped yard'
}

# Start never retires: the old unit runs with a warning until init.
assert_start_warns_until_init() {
  operator_yard -Y late integration status --json > "$STATE_ROOT/late-status.json"
  jq -e '.observed == "stopped" and (.detail | contains("run: yard -Y late init"))' "$STATE_ROOT/late-status.json" >/dev/null \
    || die 'restored selection did not warn on the stopped yard'
  operator_yard -Y late start --yes > "$STATE_ROOT/late-start.log" 2>&1
  grep -Fq 'Warning: retired integration paseo may still be installed in yard late; run: yard -Y late init' "$STATE_ROOT/late-start.log" \
    || die 'start omitted the retirement warning'
  wait_active late
  [ -z "$(retired_proof late)" ] || die 'start published retirement proof'
  operator_yard -Y late init --yes > "$STATE_ROOT/late-init.log" 2>&1
  assert_removed late
  [ "$(retired_proof late)" = true ] || die 'running init did not publish retirement proof'
  operator_yard -Y late stop --yes
}

resume_acceptance() {
  local name
  assert_state_root
  CLEANUP_ARMED=1
  assert_fixture_phase retired
  [ "$(cat /proc/sys/kernel/random/boot_id)" != "$(read_fixture_value "$BOOT_ID_STATE")" ] || die 'reboot was not observed'
  CANDIDATE_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  for name in "${YARDS[@]}"; do
    [ "$name" != late ] || continue
    for _ in $(seq 1 120); do
      [ "$(yard_power "$name")" = RUNNING ] && break
      sleep 1
    done
  done
  assert_converged
  operator_yard migrate --check --json > "$STATE_ROOT/postboot.json"
  jq -e '.outcome.code == "ready" and (.blockers | length) == 0' "$STATE_ROOT/postboot.json" >/dev/null \
    || die 'rebooted host is not ready'
  if [ "$SCENARIO" = upgrade ]; then
    assert_rollback_refused
    # After manual removal nothing retired remains to report.
    guest_yard foreign sh -eu -c '
      systemctl disable --now paseo.service
      rm -- "$1"
      systemctl daemon-reload
      rm -rf -- /srv/agents/paseo
    ' subyard "$FOREIGN_UNIT"
    operator_yard -Y foreign integration status --json > "$STATE_ROOT/status-foreign.json"
    jq -e '.observed == "ready" and (.detail // "" | contains("paseo") | not)' "$STATE_ROOT/status-foreign.json" >/dev/null \
      || die 'manual removal did not clear the foreign unit notice'
  else
    [ "$(yard_power late)" = STOPPED ] || die 'reboot started the proven stopped yard'
  fi
  operator_yard -Y default integration disable paseo --yes > "$STATE_ROOT/disable.log" 2>&1
  operator_env grep -Fq "CODING_TOOL_INTEGRATIONS='codex'" "$CONFIG_HOME/yards/default/config.env" \
    || die 'retired disable did not clear the selection'
  PRESERVE_FIXTURE=1
  printf 'ok: %s scenario of the Paseo retirement survived the reboot\n' "$SCENARIO"
}

# The migration epochs crossed by this update leave the predecessor unable to read the ledger.
assert_rollback_refused() {
  local before
  before="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  if operator_yard update --rollback --yes > "$STATE_ROOT/rollback-refused.log" 2>&1; then
    die 'rollback across the migration epochs was accepted'
  fi
  grep -Fq 'rollback-incompatible' "$STATE_ROOT/rollback-refused.log" || die 'rollback refusal omitted its code'
  [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$before" ] || die 'refused rollback changed the active release'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'refused rollback left the candidate'
}

# A release that shares the ledger accepts the rollback and reprovisions Paseo from its own assets.
assert_rollback_and_forward() {
  operator_yard update --rollback --yes > "$STATE_ROOT/rollback.log" 2>&1
  [ "$(operator_yard --version)" = "yard $BASELINE_VERSION" ] || die 'rollback did not restore the predecessor'
  operator_yard -Y default init --yes >> "$STATE_ROOT/rollback.log" 2>&1
  wait_active default
  guest_yard default sh -eu -c '[ "$(cat "$1")" = "$2" ]' subyard "$SENTINEL" "$MARKER" || die 'rollback lost the Paseo worktree'
  update_candidate --yes > "$STATE_ROOT/forward.log" 2>&1
  # Known limit: proof published before a rollback stays, so this stopped yard is not warned.
  ! grep -Fq 'yard -Y late init' "$STATE_ROOT/forward.log" || die 'stale proof no longer silences the stopped yard'
  assert_converged
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
