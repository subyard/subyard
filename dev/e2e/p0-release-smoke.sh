#!/usr/bin/env bash
# Published supported predecessor -> candidate -> reboot -> rollback on a disposable VM.
set -euo pipefail

# Reuse the existing fixture's durable state, operator and guarded cleanup.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$(dirname "${BASH_SOURCE[0]}")/power-reconciler-upgrade.sh"

# Advance this checksum-pinned baseline when the supported release baseline changes.
OLD_VERSION=0.14.0
OLD_INSTALLER_SHA256=a78c10910c0886a8d01c0a2e77718b21ce58e45fa7f0de6064bc1f351d56028b
CANDIDATE_VERSION="0.14.1-p0.smoke.$TOKEN"
if [ -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
  CANDIDATE_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
fi
INSTALLER="$STATE_ROOT/subyard-install-baseline.sh"
SMOKE_PROJECT=ReleaseSmoke
SMOKE_SOURCE="$OPERATOR_HOME/$SMOKE_PROJECT"

verify_smoke_state() {
  local version="$1"
  [ "$(operator_yard --version)" = "yard $version" ] \
    || die "unexpected active release, expected $version"
  operator_yard check
  [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = RUNNING ] \
    || die 'release smoke yard is not running'
  operator_yard shell "$SMOKE_PROJECT" --yes -- grep -Fxq "$MARKER" retained.txt
  operator_yard shell "$SMOKE_PROJECT" --yes -- grep -Fxq guest-change retained.txt
  operator_env grep -Fxq '# Release smoke retained operator setting' \
    "$OPERATOR_HOME/.config/subyard/config.env"
  if [ -f "$STATE_ROOT/checkpoint.before" ]; then
    sudo -n cmp "$STATE_ROOT/checkpoint.before" "$V2_STATE_ROOT/history-checkpoint.json" \
      || die 'release smoke changed compact history across repeat, reboot or rollback'
    sudo -n cmp "$STATE_ROOT/ledger.before" "$V2_LEDGER" \
      || die 'release smoke changed the pinned legacy ledger projection'
  fi
}

interrupt_checkpoint_update() {
  local target rc=0 checkpoint="$V2_STATE_ROOT/history-checkpoint.json"
  local observation="$OPERATOR_HOME/checkpoint-observed.json"
  # The published predecessor emits JSON for --check without a --json flag.
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --check \
    > "$STATE_ROOT/checkpoint-plan.json"
  target="$(jq -er --arg version "$CANDIDATE_VERSION" \
    '.outcome.target | select(type == "string" and startswith($version + "-") and test("^[A-Za-z0-9][A-Za-z0-9._+-]*$"))' \
    "$STATE_ROOT/checkpoint-plan.json")"
  jq -e '(.blockers // [] | length) == 0' "$STATE_ROOT/checkpoint-plan.json" >/dev/null
  assert_runtime_links "$OLD_RELEASE_TARGET" ''
  operator_env install -d -m 0700 "$OPERATOR_HOME/.config/subyard/release-transition" "$V2_STATE_ROOT"
  sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0644 \
    "$ROOT/dev/e2e/release-transition-post-cas-observer.py" "$OPERATOR_HOME/checkpoint-observer.py"
  info 'interrupting the published updater at atomic checkpoint publication before reboot'
  operator_env setsid python3 "$OPERATOR_HOME/checkpoint-observer.py" \
    --runtime-root "$OPERATOR_HOME/.subyard/runtime" --journal "$V2_JOURNAL" \
    --source-transaction none --candidate-target "$target" --checkpoint "$checkpoint" \
    --marker "$observation" --timeout 300 -- \
    env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" "$OPERATOR_HOME/.local/bin/yard" \
      update --version "$CANDIDATE_VERSION" --yes > "$STATE_ROOT/checkpoint-interruption.log" 2>&1 || rc=$?
  if [ "$rc" != 137 ] || ! operator_env test -f "$observation"; then
    cat "$STATE_ROOT/checkpoint-interruption.log" >&2
    die 'release smoke did not interrupt atomic checkpoint publication'
  fi
  sudo -n jq -e --slurpfile observed "$observation" \
    '. == $observed[0].migrationCheckpoint and .schemaVersion == 1 and (.domains | length) > 0 and all(.domains[]; .compactedThrough == .epoch and (.appliedSuffix | length) == 0)' \
    "$checkpoint" >/dev/null || die 'release smoke did not preserve the observed compact history'
  # Publication follows journal completion, but precedes final updater cleanup.
  sudo -n jq -e --arg target "$target" --slurpfile observed "$observation" \
    '.checkpoint == "complete" and .goal.direction == "activate-target" and .goal.target == $target and .releases.target == $target and .transaction == $observed[0].transaction and .checkpoint == $observed[0].checkpoint' \
    "$V2_JOURNAL" >/dev/null || die 'release smoke observed a different completed transition'
  assert_runtime_links "releases/$target" "$OLD_RELEASE_TARGET"
  sudo -n install -m 0600 "$checkpoint" "$STATE_ROOT/checkpoint.before"
  sudo -n install -m 0600 "$V2_LEDGER" "$STATE_ROOT/ledger.before"
  sudo -n install -m 0600 "$V2_JOURNAL" "$STATE_ROOT/journal.before"
}

resume_checkpoint_update() {
  sudo -n cmp "$STATE_ROOT/checkpoint.before" "$V2_STATE_ROOT/history-checkpoint.json" \
    || die 'reboot changed the interrupted compact checkpoint'
  sudo -n cmp "$STATE_ROOT/ledger.before" "$V2_LEDGER" \
    || die 'reboot changed the interrupted legacy projection'
  sudo -n cmp "$STATE_ROOT/journal.before" "$V2_JOURNAL" \
    || die 'reboot changed the interrupted transition journal'
  info 'resuming checkpoint publication in a fresh updater after reboot'
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  sudo -n jq -e --arg target "${CANDIDATE_RELEASE_TARGET#releases/}" --slurpfile before "$STATE_ROOT/journal.before" \
    '.transaction == $before[0].transaction and .checkpoint == "complete" and .goal.direction == "activate-target" and .goal.target == $target and .releases.target == $target' \
    "$V2_JOURNAL" >/dev/null || die 'post-reboot checkpoint resume did not reach its exact target'
  sudo -n cmp "$STATE_ROOT/checkpoint.before" "$V2_STATE_ROOT/history-checkpoint.json" \
    || die 'checkpoint resume changed compact history'
  sudo -n cmp "$STATE_ROOT/ledger.before" "$V2_LEDGER" \
    || die 'checkpoint resume changed the pinned legacy projection'
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  ok 'checkpoint publication interruption, reboot and fresh-process resume passed'
}

prepare_smoke() {
  local phase=smoke-ready
  prepare_fixture
  p0_capacity_reset_build_cache
  if [ -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
    # shellcheck source=tests/helpers/release-candidate.sh
    . "$ROOT/tests/helpers/release-candidate.sh"
    release_candidate_prepare "$ROOT" >/dev/null
    mkdir -p "$RELEASE_ROOT"
    cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
  else
    "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" \
      --version "$CANDIDATE_VERSION" >/dev/null
  fi
  chmod -R a+rX "$RELEASE_ROOT"
  info "installing published baseline $OLD_VERSION"
  curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
    "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" \
    -o "$INSTALLER"
  [ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
    || die 'release smoke baseline installer checksum changed'
  chmod 0755 "$INSTALLER"
  operator_env "$INSTALLER" --version "$OLD_VERSION" --yes
  printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
  incus project create "$PROJECT" \
    -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
  operator_yard init --yes
  operator_yard start --yes
  OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$OLD_RELEASE_TARGET_STATE" "$OLD_RELEASE_TARGET"
  operator_env mkdir -p "$SMOKE_SOURCE"
  operator_env bash -c 'printf "%s\n" "$2" > "$1"' _ "$SMOKE_SOURCE/retained.txt" "$MARKER"
  operator_env bash -c 'printf "\n# Release smoke retained operator setting\n" >> "$1"' _ \
    "$OPERATOR_HOME/.config/subyard/config.env"
  operator_yard sync "$SMOKE_SOURCE" --name "$SMOKE_PROJECT" --yes
  operator_yard list --live | grep -Fq "$SMOKE_PROJECT" \
    || die 'release smoke project is absent from live inventory'
  operator_yard shell "$SMOKE_PROJECT" --yes -- sh -c 'printf "guest-change\n" >> retained.txt'
  verify_smoke_state "$OLD_VERSION"

  info 'upgrading through the published updater'
  if [ -f "$ROOT/config/release-checkpoint.json" ]; then
    interrupt_checkpoint_update
    phase=smoke-checkpoint-interrupted
  else
    operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
      "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  fi
  CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE" "$CANDIDATE_RELEASE_TARGET"
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  if [ "$phase" = smoke-ready ]; then
    operator_yard init --yes
    operator_yard init --yes
    verify_smoke_state "$CANDIDATE_VERSION"
  fi
  record_reboot_baseline
  write_fixture_value "$PHASE_STATE" "$phase"
  PRESERVE_FIXTURE=1
  ok 'release smoke candidate is ready for one reboot'
}

finish_smoke() {
  local route="$STATE_ROOT/route-after" exported phase
  assert_state_root
  CLEANUP_ARMED=1
  phase="$(read_fixture_value "$PHASE_STATE")"
  case "$phase" in
    smoke-ready|smoke-checkpoint-interrupted) ;;
    *) die 'release smoke was not prepared' ;;
  esac
  [ "$(cat /proc/sys/kernel/random/boot_id)" != "$(read_fixture_value "$BOOT_ID_STATE")" ] \
    || die 'release smoke did not cross a reboot'
  ip -4 route show default > "$route"
  cmp -s "$DEFAULT_ROUTE_STATE" "$route" || die 'release smoke changed the host default route'
  load_release_targets
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  if [ "$phase" = smoke-checkpoint-interrupted ]; then
    resume_checkpoint_update
    assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
    operator_yard init --yes
    operator_yard init --yes
  fi
  verify_smoke_state "$CANDIDATE_VERSION"
  operator_yard export "$SMOKE_PROJECT" --yes
  exported="$(operator_env grep -Rl guest-change "$OPERATOR_HOME/.subyard/exports")"
  [ -n "$exported" ] || die 'release smoke export lost the guest change'
  operator_yard update --rollback --yes
  assert_runtime_links "$OLD_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  verify_smoke_state "$OLD_VERSION"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  verify_smoke_state "$CANDIDATE_VERSION"
  operator_yard remove "$SMOKE_PROJECT" --yes
  operator_yard teardown --yes
  ! incus project show "$PROJECT" >/dev/null 2>&1 || die 'release smoke teardown left its project'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] \
    || die 'last-yard teardown removed the installed runtime'
  ok 'release smoke upgrade, reboot, data preservation, rollback and teardown passed'
}

for command in sudo systemctl timeout cmp curl go incus ip jq sha256sum; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
incus info >/dev/null || die 'initialized Incus is required'

case "$MODE" in
  prepare) prepare_smoke ;;
  finish) finish_smoke ;;
  *) die 'expected prepare or finish mode' ;;
esac
