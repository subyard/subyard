#!/usr/bin/env bash
# Published anchor -> candidate, then synthetic retained AI Observer controls and retirement.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
FIXTURE_LABEL=observer-retirement
YARDS=(default solo)
DATA=/srv/agents/ai-observer/data
LOGS=(anchor-update.log solo-stop.log update-check.json update-check.txt update.log solo-follow.log solo-migrate.log
  status-default.json status-solo.json ready.json repeat-init.log repeat-migrate.log enable-refused.log postboot.json)

# shellcheck source=dev/e2e/retired-integration-fixture.sh
. "$ROOT/dev/e2e/retired-integration-fixture.sh"

seed_settings() {
  local name port
  operator_env bash -c 'sed -i "/^AGENTS=/d; /^BASE_IMAGE=/d; /^BASE_IMAGE_FALLBACK=/d; /^YARD_IMAGE=/d; /^YARD_IMAGE_FALLBACK=/d" "$1"
    printf "BASE_IMAGE=images:debian/13/cloud\nBASE_IMAGE_FALLBACK=images:debian/13/cloud\n" >> "$1"' _ "$CONFIG_HOME/config.env"
  for name in "${YARDS[@]}"; do
    case "$name" in default) port=2222 ;; solo) port=2233 ;; esac
    operator_env install -d -m 0700 "$CONFIG_HOME/yards/$name"
    operator_env bash -c 'printf "HOST_BASE=%s\nRESTRICTED_DISK_PATHS=%s\nFORWARD_SSH_AGENT=0\nSSH_PORT=%s\nCODING_TOOL_INTEGRATIONS=codex\n" \
      "$2" "$2" "$3" > "$1"; chmod 0600 "$1"' _ "$CONFIG_HOME/yards/$name/config.env" "$OPERATOR_HOME/host-data-$name" "$port"
  done
}

# The anchor no longer provisions AI Observer. After the candidate is active,
# seed the exact managed control shapes retire.py recognizes, plus retained data.
seed_observer() { # yard, host port
  local name="$1" port="$2"
  guest_yard "$name" sh -eu -c '
    managed="# Managed by Subyard AI Observer provision."
    mkdir -p /var/lib/subyard /etc/subyard/ai-observer "$2"
    for path in /usr/local/bin/ai-observer /usr/local/bin/ai-observer-check; do
      printf "#!/bin/sh\n%s\nexit 0\n" "$managed" > "$path"
      chmod 0755 "$path"
    done
    printf "%s\n" subyard-ai-observer-v1 > /etc/subyard/ai-observer/managed
    printf "%s\n" "$managed" "[Service]" "ExecStart=/usr/bin/sleep infinity" "[Install]" "WantedBy=multi-user.target" \
      > /etc/systemd/system/subyard-ai-observer.service
    chmod 0644 /etc/subyard/ai-observer/managed /etc/systemd/system/subyard-ai-observer.service
    systemctl daemon-reload
    systemctl enable --now subyard-ai-observer.service
    printf "%s\n" "$1" > "$2/.retirement-history"
    head -c 65536 /dev/urandom > "$2/ai-observer.duckdb"
    scratch=$(mktemp -d)
    printf x > "$scratch/marker"
    tar -C "$scratch" -cf - marker | docker import - subyard-e2e/ai-observer-retired:1 >/dev/null
    rm -rf -- "$scratch"
    docker create --name subyard-ai-observer --label org.subyard.managed=ai-observer-v1 \
      subyard-e2e/ai-observer-retired:1 /marker >/dev/null
  ' subyard "$MARKER" "$DATA"
  incus config set "$(instance_of "$name")" --project "$(project_of "$name")" \
    user.subyard.ai_observer_provision="$(printf '%s' "$MARKER" | sha256sum | cut -d' ' -f1)" \
    user.subyard.ai_observer_proxy="v1:$port"
  incus config device add "$(instance_of "$name")" ai-observer proxy bind=host \
    listen="tcp:127.0.0.1:$port" connect=tcp:127.0.0.1:8080 --project "$(project_of "$name")" >/dev/null
  guest_yard "$name" sh -c 'stat -c "%d:%i" "$1/ai-observer.duckdb"; sha256sum "$1/ai-observer.duckdb" "$1/.retirement-history"' \
    subyard "$DATA" > "$STATE_ROOT/data-$name.before"
}

# Old selections and the retired port setting must keep loading unchanged.
select_retired() { # yard, host port
  local name="$1" port="$2"
  operator_env bash -c 'sed -i "s/^CODING_TOOL_INTEGRATIONS=.*/CODING_TOOL_INTEGRATIONS=$2/" "$1"; printf "AI_OBSERVER_HOST_PORT=%s\n" "$3" >> "$1"' \
    _ "$CONFIG_HOME/yards/$name/config.env" "$([ "$name" = default ] && printf "'codex aiobserver'" || printf aiobserver)" "$port"
}

assert_retired() { # yard, effective selection JSON
  local name="$1" key
  guest_yard "$name" sh -eu -c '
    # set -e ignores negations and && lists, so every check fails explicitly.
    if systemctl is-active --quiet subyard-ai-observer.service; then exit 1; fi
    for path in /usr/local/bin/ai-observer /usr/local/bin/ai-observer-check /etc/subyard/ai-observer/managed \
      /etc/systemd/system/subyard-ai-observer.service /var/lib/subyard/ai-observer-retirement; do
      if [ -e "$path" ] || [ -L "$path" ]; then exit 1; fi
    done
    names=$(docker container ls -a --format "{{.Names}}")
    if printf "%s\n" "$names" | grep -Fxq subyard-ai-observer; then exit 1; fi
  ' || die "yard $name kept an AI Observer control"
  guest_yard "$name" sh -c 'stat -c "%d:%i" "$1/ai-observer.duckdb"; sha256sum "$1/ai-observer.duckdb" "$1/.retirement-history"' \
    subyard "$DATA" | cmp -s - "$STATE_ROOT/data-$name.before" || die "retirement changed retained data in yard $name"
  for key in user.subyard.ai_observer_provision user.subyard.ai_observer_proxy; do
    [ -z "$(incus config get "$(instance_of "$name")" "$key" --project "$(project_of "$name")")" ] \
      || die "yard $name kept $key"
  done
  ! incus config device list "$(instance_of "$name")" --project "$(project_of "$name")" | grep -Fxq ai-observer \
    || die "yard $name kept the AI Observer route"
  run_checked "status-$name.json" "yard $name integration status" operator_yard -Y "$name" integration status --json
  check_json "$STATE_ROOT/status-$name.json" "yard $name did not converge after retirement" \
    --argjson effective "$2" '.observed == "ready" and .selection.effective == $effective and .selection.retired == ["aiobserver"]'
  operator_env sha256sum -c "$STATE_ROOT/settings.before" >/dev/null || die 'retirement rewrote authored settings'
}

assert_all_retired() {
  assert_retired default '["codex"]'
  assert_retired solo '[]'
}

assert_ready() { # log name
  run_checked "$1" 'final readiness' operator_yard migrate --check --json
  check_json "$STATE_ROOT/$1" 'retirement did not reach ready' \
    '.outcome.status == "ready" and .outcome.reachedGoal == true and (.blockers // []) == [] and
    all(.outcome.warnings[]?; contains("aiobserver") | not)'
}

# Follow exactly the action that the update printed for the stopped yard.
follow_stopped_action() {
  local action
  action="$(tr -d '\r' < "$STATE_ROOT/update.log" | sed -n \
    's/^.*retired integration aiobserver may still be installed in yard solo; run: //p' | head -n 1)"
  # The anchor already provides disk-backed /tmp, so a stopped yard retires through init.
  case "$action" in
    'yard -Y solo init')
      run_checked solo-follow.log 'printed solo init' operator_yard -Y solo init --yes
      [ "$(yard_power solo)" = STOPPED ] || die 'retirement init did not restore stopped power'
      run_checked solo-migrate.log 'solo start after retirement' operator_yard -Y solo start --yes ;;
    *) report_failure "update printed no supported action for stopped yard solo: '$action'" "$STATE_ROOT/update.log"; return 1 ;;
  esac
}

prepare_acceptance() {
  prepare_candidate_fixture
  seed_settings
  create_yards
  update_candidate --yes > "$STATE_ROOT/anchor-update.log" 2>&1 || die 'anchor update failed before synthetic retirement setup'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate is not active before retirement setup'
  seed_observer default 22222
  seed_observer solo 22233
  # Ordinary stop is gated while retained controls await migrate. Simulate an
  # external shutdown only inside our owned namespace to test stopped retirement.
  run_checked solo-stop.log 'stop owned solo' stop_owned_solo "$(incus config get yard-solo volatile.uuid --project subyard-solo)"
  select_retired default 22222
  select_retired solo 22233
  operator_env bash -c 'sha256sum "$1"/yards/*/config.env' _ "$CONFIG_HOME" > "$STATE_ROOT/settings.before"
  # The changed selection is activation repair; migrate is its printed entry.
  operator_yard migrate --check --json > "$STATE_ROOT/update-check.json" || true
  check_json "$STATE_ROOT/update-check.json" 'AI Observer produced a migration blocker' '(.blockers // []) == []'
  operator_yard migrate --check > "$STATE_ROOT/update-check.txt" 2>&1 || true
  grep -Fq 'retired integration aiobserver may still be installed in yard solo' "$STATE_ROOT/update-check.txt" "$STATE_ROOT/update-check.json" \
    || die 'migration preview omitted the stopped yard action'
  run_checked update.log 'candidate retirement' operator_yard migrate --yes
  [ "$(yard_power solo)" = STOPPED ] || die 'migration started the stopped yard'
  assert_retired default '["codex"]'
  follow_stopped_action
  [ "$(yard_power solo)" = RUNNING ] || die 'printed action did not leave solo running'
  assert_all_retired
  assert_ready ready.json
  run_checked repeat-init.log 'repeat default init' operator_yard -Y default init --yes
  run_checked repeat-migrate.log 'repeat migration' operator_yard migrate --yes
  assert_all_retired
  if operator_yard -Y default integration enable aiobserver --yes > "$STATE_ROOT/enable-refused.log" 2>&1; then
    die 'retired integration was enabled'
  fi
  grep -Fq 'integration aiobserver is retired' "$STATE_ROOT/enable-refused.log" || die 'retired enable refusal is unclear'
  record_reboot_pending
  printf 'ok: AI Observer retirement passed; reboot pending\n'
}

resume_acceptance() {
  await_rebooted_yards
  assert_all_retired
  assert_ready postboot.json
  PRESERVE_FIXTURE=1
  printf 'ok: AI Observer retirement survived the reboot\n'
}

run_retirement_payload
