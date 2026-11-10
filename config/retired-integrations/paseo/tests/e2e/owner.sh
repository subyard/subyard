#!/usr/bin/env bash
# Published anchor -> candidate, then synthetic retained Paseo artifacts and removal.
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
YARDS=(default solo foreign)
SENTINEL=/srv/agents/paseo/worktrees/fixture/unpushed
FOREIGN_UNIT=/etc/systemd/system/paseo.service
FOREIGN_DATA=/srv/agents/paseo/data/foreign-sentinel
LOGS=(anchor-update.log update-check.json update-check.txt update.log status-default.json status-solo.json status-foreign.json
  stopped-status.json stopped-plan.log stopped-init.log stopped-repeat-init.log stopped-proven-status.json solo-start.log
  solo-stop.log stopped-before-init.json stopped-migrate.log stopped-ready.json solo-restart.log
  solo-final-stop.log solo-final-start.log
  repeat-init.log repeat-migrate.log enable-refused.log disable.log postboot.json
  final-check.json final-journal.json final-repair.log final-ready.json disable-ready.json)

report_failure() { # message, captured files...
  local message="$1" file
  shift
  printf 'paseo-removal: %s\n' "$message" >&2
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
      default) port=2222; selection=codex ;;
      solo) port=2233; selection=none ;;
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
  die 'synthetic retained Paseo did not become active'
}

seed_guests() {
  local name
  # The anchor already retires Paseo. Seed explicit leftovers only after the
  # candidate is active, so this tests its removal contract without a false
  # claim that the anchor still installs the retired package.
  for name in "${YARDS[@]}"; do
    [ "$name" = foreign ] && continue
    guest_yard "$name" sh -eu -c '
      install -d /opt/subyard/paseo /var/lib/subyard /srv/agents/paseo/worktrees
      printf "%s\n" synthetic-retained-package > /var/lib/subyard/paseo-ownership
      printf "%s\n" "[Service]" "ExecStart=/usr/bin/sleep infinity" "[Install]" "WantedBy=multi-user.target" > /etc/systemd/system/paseo.service
      chmod 0644 /etc/systemd/system/paseo.service
      systemctl daemon-reload
      systemctl enable --now paseo.service
    '
    wait_active "$name"
  done
  operator_env python3 - "$CONFIG_HOME/yards" <<'PY'
import pathlib, re, sys
for name, selection in (('default', "'codex paseo'"), ('solo', 'paseo')):
    path = pathlib.Path(sys.argv[1]) / name / 'config.env'
    path.write_text(re.sub(r'^CODING_TOOL_INTEGRATIONS=.*$', 'CODING_TOOL_INTEGRATIONS=' + selection,
                           path.read_text(), flags=re.M))
PY
  operator_env bash -c 'sha256sum "$1"/yards/*/config.env' _ "$CONFIG_HOME" > "$STATE_ROOT/settings.before"
  guest_yard default test -f /var/lib/subyard/paseo-ownership
  guest_yard default sh -eu -c 'install -d "$(dirname "$1")"; printf "%s\n" "$2" > "$1"' subyard "$SENTINEL" "$MARKER"
  # Removal also deletes this unrelated same-name service and its non-worktree data.
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
  ' || die "yard $name kept a removable Paseo file"
  if [ "$name" != solo ]; then
    guest_yard "$name" sh -c 'command -v codex >/dev/null' || die "yard $name lost selected Codex"
  fi
}

yard_power() { incus list "$(instance_of "$1")" --project "$(project_of "$1")" -f csv -c s; }
retired_proof() { incus config get "$(instance_of "$1")" user.subyard.retired_integration.paseo --project "$(project_of "$1")"; }
last_boot() { incus query "/1.0/instances/$(instance_of "$1")?project=$(project_of "$1")" | jq -er .last_used_at; }

assert_status() {
  local name="$1" effective="$2"
  run_checked "status-$name.json" "yard $name integration status" operator_yard -Y "$name" integration status --json
  check_json "$STATE_ROOT/status-$name.json" "yard $name did not converge after retirement" \
    --argjson effective "$effective" '.observed == "ready" and .selection.effective == $effective'
}

assert_converged() {
  local name
  for name in "${YARDS[@]}"; do assert_removed "$name"; done
  guest_yard default sh -eu -c '
    [ "$(cat "$1")" = "$2" ]
    [ "$(find /srv/agents/paseo -mindepth 1 -maxdepth 1 -printf "%f\n")" = worktrees ]
  ' subyard "$SENTINEL" "$MARKER" || die 'removal lost worktrees or retained other state'
  assert_status default '["codex"]'
  check_json "$STATE_ROOT/status-default.json" 'status omitted retained worktrees' \
    '.selection.retired == ["paseo"] and (.detail | contains("/srv/agents/paseo/worktrees"))'
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
  update_candidate --yes > "$STATE_ROOT/anchor-update.log" 2>&1 || die 'anchor update failed before synthetic retirement setup'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate is not active before retirement setup'
  seed_guests
  operator_yard -Y solo stop --yes
  update_candidate --check --json > "$STATE_ROOT/update-check.json" || die 'update check failed with Paseo selected'
  jq -e '(.blockers | length) == 0' "$STATE_ROOT/update-check.json" >/dev/null || die 'Paseo produced an update blocker'
  # The candidate assesses the explicitly seeded retained artifacts.
  update_candidate --check > "$STATE_ROOT/update-check.txt" 2>&1 || true
  grep -Fq 'retire integration paseo' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'candidate preview omitted the retirement'
  grep -Fq 'run: yard -Y solo start; then run yard migrate' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'update check omitted the stopped yard action'
  update_candidate --yes > "$STATE_ROOT/update.log" 2>&1 \
    || { report_failure 'candidate retirement failed' "$STATE_ROOT/update.log"; return 1; }
  grep -Fq 'Warning: yard default: retire integration paseo' "$STATE_ROOT/update.log" \
    || die 'unattended update omitted the retirement warning'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate was not activated'
  grep -Fq 'Warning: retired integration paseo may still be installed in yard solo; run: yard -Y solo start; then run yard migrate' "$STATE_ROOT/update.log" \
    || die 'unattended update omitted the stopped yard warning'
  [ "$(yard_power solo)" = STOPPED ] || die 'update started the stopped yard'
  assert_stopped_yard_finishes
  assert_converged
  run_checked repeat-init.log 'repeat default init' operator_yard -Y default init --yes
  run_checked repeat-migrate.log 'repeat migration' operator_yard migrate --yes
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

assert_stopped_yard_finishes() {
  local booted uuid
  uuid="$(incus config get yard-solo volatile.uuid --project subyard-solo)"
  [ -n "$uuid" ] || die 'solo instance UUID is missing'
  run_checked stopped-status.json 'stopped solo integration status' operator_yard -Y solo integration status --json
  check_json "$STATE_ROOT/stopped-status.json" 'stopped status omitted the combined action' \
    '.observed == "stopped" and (.detail | contains("run: yard -Y solo start; then run yard migrate"))'
  # Ordinary start has confirmation=never; EOF must not be mistaken for a decline.
  run_checked solo-start.log 'noninteractive solo start' operator_yard -Y solo start </dev/null
  [ "$(yard_power solo)" = RUNNING ] || die 'start did not leave solo running'
  [ -z "$(retired_proof solo)" ] || die 'start unexpectedly published retirement proof'
  grep -Fq 'retired integration paseo may still be installed in yard solo; run: yard migrate' "$STATE_ROOT/solo-start.log" \
    || { report_failure 'start did not name the remaining migrate step' "$STATE_ROOT/solo-start.log"; return 1; }
  [ "$(incus config get yard-solo user.subyard.disk_tmp --project subyard-solo)" = v1 ] \
    || die 'start lost the disk-/tmp marker'
  guest_yard solo sh -s -- check < "$ROOT/scripts/guest-disk-tmp.sh" || die 'start left active /tmp memory-backed'
  # Ordinary stop is gated until migrate repairs this now-running guest. Simulate an
  # external shutdown only inside our owned namespace to test stopped retirement.
  run_checked solo-stop.log 'prepare unproven stopped disk-backed solo' stop_owned_solo "$uuid"
  run_checked stopped-before-init.json 'stopped readiness before retirement init' operator_yard migrate --check --json
  check_json "$STATE_ROOT/stopped-before-init.json" 'stopped deferral did not leave init admissible' \
    '.outcome.status == "ready" and .outcome.code == "ready" and (.blockers // []) == []'
  booted="$(last_boot solo)"
  if operator_yard -Y solo init </dev/null > "$STATE_ROOT/stopped-plan.log" 2>&1; then
    die 'EOF accepted the temporary retirement start'
  fi
  grep -Fq 'temporarily start this stopped yard to retire integration paseo' "$STATE_ROOT/stopped-plan.log" \
    && grep -Fq 'chats, schedules, loops and uploads' "$STATE_ROOT/stopped-plan.log" \
    || { report_failure 'init plan omitted the temporary start or destructive retirement disclosure' "$STATE_ROOT/stopped-plan.log"; return 1; }
  [ "$(yard_power solo)" = STOPPED ] && [ "$(last_boot solo)" = "$booted" ] || die 'unconfirmed init booted solo'
  [ -z "$(retired_proof solo)" ] || die 'unconfirmed init published retirement proof'
  run_checked stopped-init.log 'retire unproven stopped solo' operator_yard -Y solo init --yes
  [ "$(yard_power solo)" = STOPPED ] || die 'retirement init did not restore stopped power'
  [ "$(retired_proof solo)" = true ] || die 'retirement init did not publish proof'
  # Once proven, ordinary stopped init must not boot the yard again.
  booted="$(last_boot solo)"
  run_checked stopped-repeat-init.log 'repeat init proven stopped solo' operator_yard -Y solo init --yes
  [ "$(yard_power solo)" = STOPPED ] && [ "$(last_boot solo)" = "$booted" ] || die 'proven init booted solo'
  ! grep -Fq 'temporarily start' "$STATE_ROOT/stopped-repeat-init.log" || die 'repeat init planned a temporary start'
  run_checked stopped-proven-status.json 'proven stopped solo status' operator_yard -Y solo integration status --json
  check_json "$STATE_ROOT/stopped-proven-status.json" 'proven stopped yard still has a retirement reminder' \
    '.observed == "stopped" and (.detail // "" | contains("retired integration paseo") | not)'
  run_checked solo-restart.log 'restart proven solo' operator_yard -Y solo start --yes
  ! grep -Fq 'retired integration paseo' "$STATE_ROOT/solo-restart.log" || die 'start warned about proven solo'
  run_checked stopped-migrate.log 'solo runtime repair after retirement' operator_yard migrate --yes
  # The printed repair sequence must leave ordinary stop usable again.
  run_checked solo-final-stop.log 'ordinary solo stop after repair' operator_yard -Y solo stop --yes
  [ "$(yard_power solo)" = STOPPED ] || die 'ready solo was not stoppable'
  run_checked solo-final-start.log 'ordinary solo start after repair' operator_yard -Y solo start --yes
  [ "$(yard_power solo)" = RUNNING ] || die 'ready solo did not restart for reboot coverage'
  run_checked stopped-ready.json 'solo final readiness' operator_yard migrate --check --json
  check_json "$STATE_ROOT/stopped-ready.json" 'start, retirement and migrate did not reach ready' \
    '.outcome.status == "ready" and .outcome.code == "ready" and .outcome.reachedGoal == true and
    (.blockers // []) == [] and all(.outcome.warnings[]?; contains("yard solo") | not)'
  [ "$(incus config get yard-solo volatile.uuid --project subyard-solo)" = "$uuid" ] \
    || die 'solo identity changed through retirement and repair'
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
    [ "$(yard_power "$name")" = RUNNING ] || die "yard $name did not return running after reboot"
  done
  assert_converged
  run_checked postboot.json 'postboot readiness' operator_yard migrate --check --json
  check_json "$STATE_ROOT/postboot.json" 'rebooted host is not ready' \
    '.outcome.status == "ready" and .outcome.code == "ready" and .outcome.reachedGoal == true and (.blockers // []) == []'
  run_checked disable.log 'disable retired selection' operator_yard -Y default integration disable paseo --yes
  operator_env grep -Fq "CODING_TOOL_INTEGRATIONS='codex'" "$CONFIG_HOME/yards/default/config.env" \
    || die 'retired disable did not clear the selection'
  run_checked disable-ready.json 'readiness after retired disable' operator_yard migrate --check --json
  check_json "$STATE_ROOT/disable-ready.json" 'retired disable left release unready' \
    '.outcome.status == "ready" and .outcome.code == "ready" and .outcome.reachedGoal == true and (.blockers // []) == []'
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
