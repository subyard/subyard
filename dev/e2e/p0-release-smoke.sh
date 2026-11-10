#!/usr/bin/env bash
# Published supported predecessor -> candidate -> reboot -> rollback on a disposable VM.
set -euo pipefail

# Reuse the existing fixture's durable state, operator and guarded cleanup.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$(dirname "${BASH_SOURCE[0]}")/power-reconciler-upgrade.sh"

# The shared fixture reads the TSV anchor baseline.
CANDIDATE_VERSION="0.28.0-p0.smoke.$TOKEN"
if [ -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
  CANDIDATE_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
fi
COMPATIBLE_VERSION="$CANDIDATE_VERSION-p0.compatible.$TOKEN"
COMPATIBLE_RELEASE_ROOT="$STATE_ROOT/compatible-release"
INSTALLER="$STATE_ROOT/subyard-install-baseline.sh"
SMOKE_PROJECT=ReleaseSmoke
SMOKE_SOURCE="$OPERATOR_HOME/$SMOKE_PROJECT"

verify_smoke_state() {
  local version="$1"
  [ "$(operator_yard --version)" = "yard $version" ] \
    || die "unexpected active release, expected $version"
  if [ "$version" != "$OLD_VERSION" ]; then assert_registered_ownership; fi
  operator_yard check
  [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = RUNNING ] \
    || die 'release smoke yard is not running'
  operator_yard shell "$SMOKE_PROJECT" --yes -- grep -Fxq "$MARKER" retained.txt
  operator_yard shell "$SMOKE_PROJECT" --yes -- grep -Fxq guest-change retained.txt
  operator_env grep -Fxq '# Release smoke retained operator setting' \
    "$OPERATOR_HOME/.config/subyard/config.env"

}

establish_smoke_ownership() {
  local phase state="$STATE_ROOT/ownership-enrollment" runtime="$OPERATOR_HOME/.subyard/runtime"
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  assert_registered_ownership
  # Historical public installers predate the runtime ownership inventory.
  # Stage the same verified release through the current public installer only
  # after the authentic update is ready; this changes no yard ownership.
  for phase in before after; do
    {
      sudo -n stat -c '%n:%f:%u:%g:%N' "$runtime/current" "$runtime/previous"
      sudo -n find "$V2_STATE_ROOT" -printf '%P:%y:%m:%U:%G:%l\n'
    } | LC_ALL=C sort > "$state.$phase.metadata"
    sudo -n find "$V2_STATE_ROOT" -type f -exec sha256sum -- {} + \
      | LC_ALL=C sort > "$state.$phase.sha256"
    if [ "$phase" = before ]; then
      operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
        "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --publish-only --yes
    fi
  done
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  cmp -s "$state.before.metadata" "$state.after.metadata" \
    && cmp -s "$state.before.sha256" "$state.after.sha256" \
    || die 'release smoke ownership enrollment changed protected history or runtime links'
  establish_registered_ownership
}

verify_predecessor_rollback_refusal() {
  local path
  for path in "$V2_STATE_ROOT/history-checkpoint.json" "$V2_LEDGER" "$V2_JOURNAL"; do
    sudo -n install -m 0600 "$path" "$STATE_ROOT/rollback-refusal-${path##*/}"
  done
  if operator_yard update --rollback --yes > "$STATE_ROOT/predecessor-rollback.log" 2>&1; then
    die 'release smoke allowed incompatible published predecessor rollback'
  fi
  grep -Fq 'code=rollback-incompatible' "$STATE_ROOT/predecessor-rollback.log" \
    || die 'release smoke predecessor rollback failed for an unexpected reason'
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  for path in "$V2_STATE_ROOT/history-checkpoint.json" "$V2_LEDGER" "$V2_JOURNAL"; do
    sudo -n cmp -s "$STATE_ROOT/rollback-refusal-${path##*/}" "$path" \
      || die "refused predecessor rollback changed ${path##*/}"
  done
  ok 'incompatible published predecessor rollback refused without mutation'
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
  # A separate standard package retains the same current activation owners.
  # The authentic predecessor remains the upgrade/checkpoint baseline only.
  "$ROOT/dev/package-engine.sh" --output-dir "$COMPATIBLE_RELEASE_ROOT" \
    --version "$COMPATIBLE_VERSION" >/dev/null
  chmod -R a+rX "$COMPATIBLE_RELEASE_ROOT"
  info "installing published baseline $OLD_VERSION"
  curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
    "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" \
    -o "$INSTALLER"
  [ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
    || die 'release smoke baseline installer checksum changed'
  chmod 0755 "$INSTALLER"
  operator_env "$INSTALLER" --version "$OLD_VERSION" --yes
  printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
  if ! operator_yard init --yes > "$STATE_ROOT/baseline-init.log" 2>&1; then
    incus project show "$PROJECT" >/dev/null 2>&1 \
      && [ -z "$(incus list --project "$PROJECT" --format csv -c n)" ] \
      || die 'baseline init failed before its image step'
    incus project set "$PROJECT" features.images=false
    operator_yard init --yes
  fi
  incus project set "$PROJECT" user.p0-power-systemd="$MARKER"
  operator_yard start --yes
  OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$OLD_RELEASE_TARGET_STATE" "$OLD_RELEASE_TARGET"
  operator_env mkdir -p "$SMOKE_SOURCE"
  operator_env bash -c 'printf "%s\n" "$2" > "$1"' _ "$SMOKE_SOURCE/retained.txt" "$MARKER"
  operator_env bash -c 'printf "\n# Release smoke retained operator setting\n" >> "$1"' _ \
    "$OPERATOR_HOME/.config/subyard/config.env"
  operator_yard sync "$SMOKE_SOURCE" --name "$SMOKE_PROJECT" --yes
  operator_yard list --live | grep -F "$SMOKE_PROJECT" >/dev/null \
    || die 'release smoke project is absent from live inventory'
  operator_yard shell "$SMOKE_PROJECT" --yes -- sh -c 'printf "guest-change\n" >> retained.txt'
  verify_smoke_state "$OLD_VERSION"

  info 'upgrading through the published updater'
  # The anchor already owns a compact checkpoint. Its reader/projection is
  # preserved; publication-cut contracts remain in the host-free reader tests.
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE" "$CANDIDATE_RELEASE_TARGET"
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  if [ "$phase" = smoke-ready ]; then
    establish_smoke_ownership
    operator_yard init --yes
    verify_smoke_state "$CANDIDATE_VERSION"
  fi
  record_reboot_baseline
  write_fixture_value "$PHASE_STATE" "$phase"
  PRESERVE_FIXTURE=1
  ok 'release smoke candidate is ready for one reboot'
}

finish_smoke() {
  local route="$STATE_ROOT/route-after" exported phase compatible_target
  assert_state_root
  CLEANUP_ARMED=1
  phase="$(read_fixture_value "$PHASE_STATE")"
  case "$phase" in
    smoke-ready) ;;
    *) die 'release smoke was not prepared' ;;
  esac
  [ "$(cat /proc/sys/kernel/random/boot_id)" != "$(read_fixture_value "$BOOT_ID_STATE")" ] \
    || die 'release smoke did not cross a reboot'
  ip -4 route show default > "$route"
  cmp -s "$DEFAULT_ROUTE_STATE" "$route" || die 'release smoke changed the host default route'
  load_release_targets
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"

  verify_smoke_state "$CANDIDATE_VERSION"
  operator_yard export "$SMOKE_PROJECT" --yes
  exported="$(operator_env grep -Rl guest-change "$OPERATOR_HOME/.subyard/exports")"
  [ -n "$exported" ] || die 'release smoke export lost the guest change'
  # Establish current=A/previous=B using only verified public update operations.
  # This leaves the exact candidate active for its compatible rollback cycle.
  operator_env env YARD_RELEASE_BASE_URL="file://$COMPATIBLE_RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$COMPATIBLE_VERSION" --yes
  compatible_target="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  assert_runtime_links "$compatible_target" "$CANDIDATE_RELEASE_TARGET"
  verify_smoke_state "$COMPATIBLE_VERSION"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$compatible_target"
  verify_smoke_state "$CANDIDATE_VERSION"
  operator_yard update --rollback --yes
  assert_runtime_links "$compatible_target" "$CANDIDATE_RELEASE_TARGET"
  verify_smoke_state "$COMPATIBLE_VERSION"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$compatible_target"
  verify_smoke_state "$CANDIDATE_VERSION"
  operator_yard remove "$SMOKE_PROJECT" --yes
  operator_yard teardown --yes
  ! incus project show "$PROJECT" >/dev/null 2>&1 || die 'release smoke teardown left its project'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] \
    || die 'last-yard teardown removed the installed runtime'
  ok 'release smoke upgrade, reboot, data preservation, rollback refusal, compatible rollback/roll-forward and teardown passed'
}

for command in sudo systemctl timeout cmp curl go incus ip jq rg sha256sum; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
incus info >/dev/null || die 'initialized Incus is required'

case "$MODE" in
  prepare) prepare_smoke ;;
  finish) finish_smoke ;;
  *) die 'expected prepare or finish mode' ;;
esac
