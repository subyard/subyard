#!/usr/bin/env bash
# Synthetic unmarked-native continuation lifecycle in a verified installed release.
set -euo pipefail
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$(dirname "${BASH_SOURCE[0]}")/power-reconciler-upgrade.sh"

assert_legacy_continuation() {
  local grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json" uuid
  assert_project_owned
  [ -z "$(incus project get "$PROJECT" user.subyard.installation)" ] \
    || die 'legacy continuation acquired destructive installation ownership'
  operator_env test -f "$grant" && operator_env test ! -L "$grant" \
    && [ "$(operator_env stat -c '%a:%u' "$grant")" = "600:$(operator_uid)" ] \
    && [ "$(operator_env sha256sum "$grant" | awk '{print $1}')" = "$(cat "$STATE_ROOT/legacy-continuation.sha256")" ] \
    || die 'protected continuation grant changed across init, reboot or release links'
  uuid="$(operator_env jq -er --argjson uid "$(operator_uid)" \
    'select(.schema == 1 and .scope == "normal-reconcile" and .operatorUid == $uid) | .instanceUuid | select(type == "string" and length > 0)' "$grant")" \
    || die 'continuation grant has an invalid identity'
  [ "$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")" = "$uuid" ] \
    || die 'continuation changed the native instance UUID'
}

# Preparation initially proves registered ownership. After the explicit synthetic
# boundary, every shared rollback/native-policy assertion instead proves the grant.
assert_upgrade_ownership() {
  if [ -e "$STATE_ROOT/legacy-continuation.sha256" ]; then
    assert_legacy_continuation
  else
    assert_registered_ownership
  fi
}

verify_legacy_full_removal_refusal() {
  local refusal="$STATE_ROOT/continuation-full-removal-refusal.log" status=0
  operator_yard teardown --full --yes > "$refusal" 2>&1 || status=$?
  [ "$status" != 0 ] || die 'continuation-only installation unexpectedly accepted full removal'
  grep -Fq 'legacy continuation grants no full removal ownership' "$refusal" \
    || die 'full removal did not identify the continuation-only boundary'
  incus info "$INSTANCE" --project "$PROJECT" >/dev/null \
    || die 'full removal refusal changed the retained legacy instance'
  assert_legacy_continuation
}

establish_legacy_continuation() {
  local grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  operator_yard init --continue-legacy --yes
  write_fixture_value "$STATE_ROOT/legacy-continuation.sha256" \
    "$(operator_env sha256sum "$grant" | awk '{print $1}')"
  assert_legacy_continuation
  verify_legacy_full_removal_refusal
  operator_yard init --yes
  assert_legacy_continuation
}

seed_unmarked_continuation() {
  local project_seed
  assert_project_owned
  assert_registered_ownership
  # Match yard-adoption's explicit synthetic boundary. Keep the independent
  # fixture cleanup marker, instance data/UUID and completed installed history.
  project_seed="$(incus query "/1.0/projects/$PROJECT" | jq -c \
    '{description,config:(.config|with_entries(select(.key|startswith("user.subyard.")|not)))}')"
  incus query --request PUT "/1.0/projects/$PROJECT" --data "$project_seed" >/dev/null
  assert_project_owned
  if operator_yard init --yes > "$STATE_ROOT/continuation-unconfirmed.log" 2>&1; then
    die 'plain init silently authorized the synthetic unmarked project'
  fi
  grep -Fq 'existing unmarked project has no continuation consent' "$STATE_ROOT/continuation-unconfirmed.log" \
    || die 'plain init refused for a reason other than missing continuation consent'
  establish_legacy_continuation
  printf 'evidence: continuation_seed=synthetic-unmarked-native installed_history=completed\n'
}

verify_continuation_init() {
  assert_legacy_continuation
  operator_yard init --yes
  operator_yard init --yes
  assert_legacy_continuation
  verify_legacy_full_removal_refusal
  assert_candidate_state
}

# Definitions can be extracted by host-free fixture contracts.
if [ "${BASH_SOURCE[0]}" != "$0" ]; then return 0; fi
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] || die 'a frozen candidate bundle is required'
case "$MODE" in
  prepare)
    prepare_candidate
    seed_unmarked_continuation
    verify_continuation_init
    record_reboot_baseline
    write_fixture_value "$PHASE_STATE" candidate-ready
    PRESERVE_FIXTURE=1
    ;;
  resume)
    assert_state_root
    CLEANUP_ARMED=1
    assert_fixture_phase candidate-ready
    assert_legacy_continuation
    assert_post_reboot_candidate
    verify_continuation_init
    record_reboot_baseline
    write_fixture_value "$PHASE_STATE" candidate-reconciled
    PRESERVE_FIXTURE=1
    ;;
  finish)
    assert_state_root
    CLEANUP_ARMED=1
    assert_fixture_phase candidate-reconciled
    assert_legacy_continuation
    assert_post_reboot_candidate
    verify_continuation_init
    finish_candidate_flow
    assert_legacy_continuation
    verify_legacy_full_removal_refusal
    printf 'ok: continuation grant, UUID and native policy survived repeated init, two reboots and compatible rollback/forward\n'
    ;;
  *) die 'expected prepare, resume or finish mode' ;;
esac
