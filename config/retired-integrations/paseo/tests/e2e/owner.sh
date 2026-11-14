#!/usr/bin/env bash
# Published anchor -> candidate, then synthetic retained Paseo artifacts and removal.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
FIXTURE_LABEL=paseo-removal
YARDS=(default solo foreign)
SENTINEL=/srv/agents/paseo/worktrees/fixture/unpushed
FOREIGN_UNIT=/etc/systemd/system/paseo.service
FOREIGN_DATA=/srv/agents/paseo/data/foreign-sentinel
LOGS=(anchor-update.log update-check.json update-check.txt update.log status-default.json status-solo.json status-foreign.json
  stopped-status.json stopped-plan.log stopped-init.log stopped-repeat-init.log stopped-proven-status.json solo-start.log
  solo-stop.log stopped-before-init.json stopped-migrate.log stopped-ready.json solo-restart.log
  solo-final-stop.log solo-final-start.log
  repeat-init.log repeat-migrate.log enable-refused.log disable.log postboot.json
  disable-ready.json)

# shellcheck source=dev/e2e/retired-integration-fixture.sh
. "$ROOT/dev/e2e/retired-integration-fixture.sh"

seed_settings() {
  local name port selection
  # Each baseline creates its own project/image store. The prepared alias lives
  # in project default; use the remote image supported by both published owners.
  operator_env bash -c 'sed -i "/^AGENTS=/d; /^BASE_IMAGE=/d; /^BASE_IMAGE_FALLBACK=/d; /^YARD_IMAGE=/d; /^YARD_IMAGE_FALLBACK=/d" "$1"
    printf "BASE_IMAGE=images:debian/13/cloud\nBASE_IMAGE_FALLBACK=images:debian/13/cloud\n" >> "$1"' _ "$CONFIG_HOME/config.env"
  for name in "${YARDS[@]}"; do
    case "$name" in
      default) port=2222; selection=codex ;;
      solo) port=2233; selection= ;;
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

# Select the retired package only after solo is stopped: a changed selection
# makes activation repair pending, which gates ordinary stop until migrate.
select_retired() {
  operator_env python3 - "$CONFIG_HOME/yards" <<'PY'
import pathlib, re, sys
for name, selection in (('default', "'codex paseo'"), ('solo', 'paseo')):
    path = pathlib.Path(sys.argv[1]) / name / 'config.env'
    path.write_text(re.sub(r'^CODING_TOOL_INTEGRATIONS=.*$', 'CODING_TOOL_INTEGRATIONS=' + selection,
                           path.read_text(), flags=re.M))
PY
  operator_env bash -c 'sha256sum "$1"/yards/*/config.env' _ "$CONFIG_HOME" > "$STATE_ROOT/settings.before"
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

prepare_acceptance() {
  prepare_candidate_fixture
  seed_settings
  create_yards
  update_candidate --yes > "$STATE_ROOT/anchor-update.log" 2>&1 || die 'anchor update failed before synthetic retirement setup'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate is not active before retirement setup'
  seed_guests
  operator_yard -Y solo stop --yes
  select_retired
  # The changed selection is activation repair; migrate is its printed entry.
  operator_yard migrate --check --json > "$STATE_ROOT/update-check.json" || true
  check_json "$STATE_ROOT/update-check.json" 'Paseo produced a migration blocker' '(.blockers // []) == []'
  operator_yard migrate --check > "$STATE_ROOT/update-check.txt" 2>&1 || true
  grep -Fq 'retire integration paseo' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'migration preview omitted the retirement'
  grep -Fq 'run: yard -Y solo start; then run yard migrate' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'migration preview omitted the stopped yard action'
  run_checked update.log 'candidate retirement' operator_yard migrate --yes
  grep -Fq 'retire integration paseo' "$STATE_ROOT/update.log" || die 'unattended migration omitted the retirement warning'
  grep -Fq 'retired integration paseo may still be installed in yard solo; run: yard -Y solo start; then run yard migrate' "$STATE_ROOT/update.log" \
    || die 'unattended migration omitted the stopped yard warning'
  [ "$(yard_power solo)" = STOPPED ] || die 'migration started the stopped yard'
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
  record_reboot_pending
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
  await_rebooted_yards
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

run_retirement_payload
