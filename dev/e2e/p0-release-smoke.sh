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
}

prepare_smoke() {
  prepare_fixture
  p0_capacity_reset_build_cache
  "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" \
    --version "$CANDIDATE_VERSION" >/dev/null
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
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE" "$CANDIDATE_RELEASE_TARGET"
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  operator_yard init --yes
  operator_yard init --yes
  verify_smoke_state "$CANDIDATE_VERSION"
  record_reboot_baseline
  write_fixture_value "$PHASE_STATE" smoke-ready
  PRESERVE_FIXTURE=1
  ok 'release smoke candidate is ready for one reboot'
}

finish_smoke() {
  local route="$STATE_ROOT/route-after" exported
  assert_state_root
  CLEANUP_ARMED=1
  [ "$(read_fixture_value "$PHASE_STATE")" = smoke-ready ] \
    || die 'release smoke was not prepared'
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
