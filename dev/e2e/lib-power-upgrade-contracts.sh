#!/usr/bin/env bash
# Shared assertions and state helpers for the power-upgrade and release-smoke fixtures.
# Definitions only: callers own initialization, lease validation and cleanup traps.

operator_env() {
  local uid
  uid="$(operator_uid)"
  sudo -n /usr/sbin/runuser -u "$OPERATOR" -- env \
    -u SUDO_USER -u SUDO_UID -u SUDO_GID -u SUDO_COMMAND \
    HOME="$OPERATOR_HOME" USER="$OPERATOR" LOGNAME="$OPERATOR" SHELL=/bin/bash \
    PATH="$OPERATOR_HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    GOCACHE="$OPERATOR_HOME/.cache/go-build" GOMODCACHE="$OPERATOR_HOME/go/pkg/mod" \
    XDG_RUNTIME_DIR="/run/user/$uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    bash -c 'cd "$HOME" && exec "$@"' _ "$@"
}

materialize_unit() {
  local template="$1" output="$2" runtime_root contracts
  local -a reader=()
  case "$template" in
    "$OPERATOR_HOME/.subyard/runtime/releases/"*/config/systemd/subyard-power-reconcile.service.in)
      reader=(operator_env)
      runtime_root="${template%/config/systemd/subyard-power-reconcile.service.in}" ;;
    "$OLD_UNIT_FIXTURE") runtime_root='' ;;
    "$ROOT/config/systemd/subyard-power-reconcile.service.in")
      runtime_root="$OPERATOR_HOME/.subyard/runtime/${CANDIDATE_RELEASE_TARGET:-}" ;;
    *) die 'unsupported power unit template context' ;;
  esac
  "${reader[@]}" sed "s|@SUBYARD_POWER_RECONCILER@|$RECONCILER|g" "$template" > "$output"
  [ "$template" != "$OLD_UNIT_FIXTURE" ] || return 0
  # Current templates bind to the captured immutable candidate, including when
  # the controller-owned source template is used to verify rollback's boot unit.
  case "$runtime_root" in
    "$OPERATOR_HOME/.subyard/runtime/releases/"?*) ;;
    *) die 'candidate power unit runtime is unavailable' ;;
  esac
  operator_env test -f "$runtime_root/bin/yard-engine" \
    && operator_env test ! -L "$runtime_root/bin/yard-engine" \
    && operator_env test -x "$runtime_root/bin/yard-engine" \
    || die 'candidate power unit engine is missing or unsafe'
  contracts="$(operator_env env SUBYARD_REPOSITORY_ROOT="$runtime_root" \
    SUBYARD_CONFIG_HOME="$OPERATOR_HOME/.config/subyard" \
    "$runtime_root/bin/yard-engine" _publication-contracts)" \
    || die 'candidate power unit publication snapshot is unavailable'
  [[ "$contracts" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || die 'candidate power unit publication snapshot is invalid'
  printf '\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=%s\n' "$contracts" >> "$output"
}

assert_activation_only_journal() {
  local checkpoint="$1" transaction="$2" authorization="$3"
  local target="${CANDIDATE_RELEASE_TARGET#releases/}"
  operator_env jq -e \
    --arg checkpoint "$checkpoint" \
    --arg transaction "$transaction" \
    --arg authorization "$authorization" \
    --arg target "$target" '
      .schemaVersion == 2 and
      .checkpoint == $checkpoint and
      .transaction == $transaction and
      .authorizationDigest == $authorization and
      .goal == {"target": $target, "direction": "activate-target"} and
      .releases.from == $target and .releases.target == $target and
      (.steps | length) == 0
    ' "$V2_JOURNAL" >/dev/null \
    || die "activation-only journal is not at checkpoint $checkpoint"
}

assert_activation_ledger_unchanged() {
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER" \
    || die 'activation-only repair changed the exact v2 ledger bytes'
}

install_activation_reconcile_fault() {
  local temporary
  temporary="$(mktemp "$STATE_ROOT/.activation-systemctl.XXXXXX")"
  printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    "journal=$V2_JOURNAL" \
    "probe=$ACTIVATION_FAULT_PROBE" \
    "delegate=$ACTIVATION_SYSTEMCTL_DELEGATE" \
    'if [ "${1:-}" = show ] &&' \
    '  [ "${2:-}" = subyard-power-reconcile.service ] &&' \
    '  /usr/bin/jq -e ".schemaVersion == 2 and .checkpoint == \"reconciling\"" "$journal" >/dev/null 2>&1; then' \
    '  failures=0' \
    '  [ ! -r "$probe" ] || failures=$(wc -l < "$probe")' \
    '  if [ "$failures" = 0 ]; then' \
    '    printf "%s\n" "injected activation reconciler post-apply interruption" >&2' \
    '    printf "%s\n" "reconciling post-apply failure" >> "$probe"' \
    '    exit 75' \
    '  fi' \
    'fi' \
    'exec "$delegate" "$@"' \
    > "$temporary"
  sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0755 \
    "$temporary" "$ACTIVATION_SYSTEMCTL_WRAPPER"
  find "$temporary" -delete
}

exercise_activation_only_repair() {
  local completed_transaction repair_transaction repair_authorization
  local candidate_unit="$STATE_ROOT/candidate-activation.service"
  local drift_unit="$STATE_ROOT/drifted-activation.service"
  local unconfirmed_output unconfirmed_rc interrupted_output interrupted_rc
  local fault_count resumed_output resumed_rc

  load_release_targets
  completed_transaction="$(operator_env jq -er '.transaction' "$V2_JOURNAL")" \
    || die 'completed candidate transaction is unavailable'
  sudo -n install -m 0600 "$V2_LEDGER" "$ACTIVATION_LEDGER_BASELINE"
  materialize_unit "$OPERATOR_HOME/.subyard/runtime/$CANDIDATE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" "$candidate_unit"
  materialize_unit "$OLD_UNIT_FIXTURE" "$drift_unit"
  sudo -n install -o root -g root -m 0644 "$drift_unit" "$UNIT"
  sudo -n systemctl daemon-reload
  sudo -n cmp "$drift_unit" "$UNIT" >/dev/null \
    || die 'release-owned activation drift was not installed exactly'
  if sudo -n cmp -s "$candidate_unit" "$UNIT"; then
    die 'release-owned activation resource did not enter drift'
  fi

  sudo -n install -m 0600 "$V2_JOURNAL" "$ACTIVATION_JOURNAL_BASELINE"
  set +e
  unconfirmed_output="$(
    operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
      "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" \
      </dev/null 2>&1
  )"
  unconfirmed_rc=$?
  set -e
  [ "$unconfirmed_rc" = 1 ] \
    && [ "$(grep -Fc 'confirmation required: interactive terminal required' \
      <<<"$unconfirmed_output")" = 1 ] \
    || die "unconfirmed activation repair did not stop at confirmation: $unconfirmed_output"
  sudo -n cmp "$ACTIVATION_JOURNAL_BASELINE" "$V2_JOURNAL" \
    || die 'unconfirmed activation repair changed the exact journal bytes'
  sudo -n cmp "$drift_unit" "$UNIT" \
    || die 'unconfirmed activation repair changed the release-owned resource'
  assert_activation_ledger_unchanged

  install_activation_reconcile_fault
  set +e
  interrupted_output="$(
    operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
      "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes \
      </dev/null 2>&1
  )"
  interrupted_rc=$?
  set -e
  [ "$interrupted_rc" -ne 0 ] \
    || die 'injected activation reconciliation unexpectedly completed'
  if ! operator_env test -f "$ACTIVATION_FAULT_PROBE"; then
    printf '%s\n' "$interrupted_output" >&2
    die 'activation reconciliation did not reach the injected post-apply checkpoint'
  fi
  fault_count="$(operator_env wc -l "$ACTIVATION_FAULT_PROBE" | awk '{print $1}')"
  [ "$fault_count" = 1 ] \
    || die "activation reconciliation injected failures=$fault_count, want exactly 1"
  ! grep -Fq 'confirmation required: interactive terminal required' <<<"$interrupted_output" \
    || die 'authorized activation reconciliation requested confirmation again'
  operator_env find "$ACTIVATION_SYSTEMCTL_WRAPPER" -delete

  repair_transaction="$(operator_env jq -er '.transaction' "$V2_JOURNAL")" \
    || die 'activation repair transaction is unavailable after interruption'
  repair_authorization="$(operator_env jq -er '.authorizationDigest' "$V2_JOURNAL")" \
    || die 'activation repair authorization is unavailable after interruption'
  [ "$repair_transaction" != "$completed_transaction" ] \
    || die 'activation drift reused the completed migration transaction'
  assert_activation_only_journal reconciling "$repair_transaction" "$repair_authorization"
  assert_activation_ledger_unchanged
  sudo -n cmp "$candidate_unit" "$UNIT" >/dev/null \
    || die 'interrupted reconciler did not restore the exact candidate activation resource'

  if resumed_output="$(
    operator_yard update --version "$CANDIDATE_VERSION" </dev/null 2>&1
  )"; then
    :
  else
    resumed_rc=$?
    printf '%s\n' "$resumed_output" >&2
    die "activation-only same-version update did not resume (exit=$resumed_rc)"
  fi
  ! grep -Eq 'Proceed\?|confirmation required' <<<"$resumed_output" \
    || die 'activation-only resume requested a second confirmation'
  assert_activation_only_journal complete "$repair_transaction" "$repair_authorization"
  assert_activation_ledger_unchanged
  assert_candidate_state
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  ok 'same-version activation-only repair resumed without replaying migration history'
}

assert_v2_transition() {
  local direction="$1" target="${2#releases/}" expected_catalog="${3:-}"
  local runtime="$OPERATOR_HOME/.subyard/runtime" artifact_digest registry_digest record
  artifact_digest="$(operator_env sha256sum "$runtime/releases/$target/runtime-files.sha256" | awk '{print $1}')" \
    || die 'target artifact manifest is unavailable'
  registry_digest="$(operator_env sha256sum "$runtime/$CANDIDATE_RELEASE_TARGET/config/release-transition.json" | awk '{print $1}')" \
    || die 'candidate transition owner registry is unavailable'
  operator_env jq -e '
    .schemaVersion == 2 and
    .domains["owner-registration"] == {
      "epoch": 2, "applied": ["canonicalize-test-yard-owner-v2"]
    } and
    .domains["power-metadata"] == {"epoch": 2, "applied": ["introduce-owner-publication-state-v1"]} and
    .domains["project-state"] == {"epoch": 1, "applied": []} and
    .domains.settings == {
      "epoch": 3, "applied": ["canonicalize-test-vms-settings-v2", "localize-ssh-port-settings-v3"]
    }
  ' "$OPERATOR_HOME/.config/subyard/release-transition/v2/ledger.json" >/dev/null \
    || {
      # Capture allowlisted migration facts before marker-owned cleanup removes them.
      for record in ledger.json history-checkpoint.json; do
        printf 'power-reconciler-upgrade: migration record=%s\n' "$record" >&2
        operator_env jq -c '{schemaVersion, registryDigest, legacyProjection, domains}' \
          "$OPERATOR_HOME/.config/subyard/release-transition/v2/$record" >&2 \
          || printf 'power-reconciler-upgrade: migration record unavailable=%s\n' "$record" >&2
      done
      die 'candidate v2 migration ledger is not at its exact per-domain fixed point'
    }
  operator_env jq -e \
    --arg direction "$direction" --arg target "$target" \
    --arg artifact "$artifact_digest" --arg registry "$registry_digest" \
    --arg catalog "$expected_catalog" '
    .schemaVersion == 2 and .checkpoint == "complete" and
    .goal == {"target": $target, "direction": $direction} and
    .releases.target == $target and
    (has("rollbackTarget") | not) and
    .artifactDigest == $artifact and .registryDigest == $registry and
    (.catalogDigest | type == "string" and test("^[a-f0-9]{64}$")) and
    ($direction != "activate-previous" or .catalogDigest == $catalog) and
    (.steps | type == "array") and all(.steps[]; .checkpoint == "verified")
  ' "$OPERATOR_HOME/.config/subyard/release-transition/v2/journal.json" >/dev/null \
    || die 'candidate v2 transition journal is not complete for the exact release goal'
}

write_fixture_value() {
  local path="$1" value="$2" temporary
  case "$path" in
    "$STATE_ROOT"/*) ;;
    *) die "refusing fixture state outside $STATE_ROOT: $path" ;;
  esac
  temporary="$(mktemp "$STATE_ROOT/.state.XXXXXX")"
  printf '%s\n' "$value" > "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$path"
}

read_fixture_value() {
  local path="$1" value lines
  [ -f "$path" ] && [ ! -L "$path" ] \
    || die "fixture state is missing or unsafe: $path"
  lines="$(wc -l < "$path")"
  [ "$lines" = 1 ] || die "fixture state is not one line: $path"
  IFS= read -r value < "$path"
  [ -n "$value" ] || die "fixture state is empty: $path"
  printf '%s\n' "$value"
}

assert_fixture_phase() {
  local expected="$1" actual
  actual="$(read_fixture_value "$PHASE_STATE")"
  [ "$actual" = "$expected" ] \
    || die "fixture phase is $actual, expected $expected"
}

assert_post_reboot_candidate() {
  local previous_boot current_boot current_route manager_state started instance_state
  previous_boot="$(read_fixture_value "$BOOT_ID_STATE")"
  current_boot="$(cat /proc/sys/kernel/random/boot_id)"
  [ -n "$current_boot" ] && [ "$current_boot" != "$previous_boot" ] \
    || die 'power reconciler fixture did not cross a host reboot'
  [ -f "$DEFAULT_ROUTE_STATE" ] && [ ! -L "$DEFAULT_ROUTE_STATE" ] \
    || die 'pre-reboot default-route state is missing or unsafe'
  current_route="$(mktemp "$STATE_ROOT/.default-route-current.XXXXXX")"
  ip -4 route show default > "$current_route"
  if ! cmp -s "$DEFAULT_ROUTE_STATE" "$current_route"; then
    printf 'power-reconciler-upgrade: default route changed across reboot\n' >&2
    diff -u "$DEFAULT_ROUTE_STATE" "$current_route" >&2 || true
    find "$current_route" -delete
    return 2
  fi
  find "$current_route" -delete

  load_release_targets
  assert_candidate_state
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  manager_state="$(sudo -n systemctl show subyard-power-reconcile.service \
    --property=LoadState --property=NeedDaemonReload \
    --property=ActiveState --property=SubState --property=Result \
    --property=ExecMainStatus --property=ExecMainStartTimestampMonotonic)" \
    || die 'cannot observe current-boot power reconciler result'
  started="$(sed -n 's/^ExecMainStartTimestampMonotonic=//p' <<<"$manager_state")"
  grep -Fxq 'LoadState=loaded' <<<"$manager_state" \
    && grep -Fxq 'NeedDaemonReload=no' <<<"$manager_state" \
    && grep -Fxq 'ActiveState=inactive' <<<"$manager_state" \
    && grep -Fxq 'SubState=dead' <<<"$manager_state" \
    && grep -Fxq 'Result=success' <<<"$manager_state" \
    && grep -Fxq 'ExecMainStatus=0' <<<"$manager_state" \
    && [[ "$started" =~ ^[1-9][0-9]*$ ]] \
    || die "candidate did not reconcile successfully in the current boot: ${manager_state//$'\n'/, }"
  instance_state="$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)"
  [ "$instance_state" = RUNNING ] \
    || die "desired-running yard is $instance_state after reboot"
}

project_presence() {
  local projects
  projects="$(incus project list --format csv -c n)" || return 2
  grep -Fxq "$PROJECT" <<<"$projects" && return 0
  return 1
}

capture_legacy_native_policy() {
  local output="$1" revisions
  revisions="$(jq -s '[.[].instance_publication.revision_key // empty]' "$ROOT"/config/profiles/*/profile.json)"
  incus query "/1.0/projects/$PROJECT" > "$output.project"
  incus query "/1.0/profiles/default?project=$PROJECT" > "$output.profile"
  incus query "/1.0/instances/$INSTANCE?project=$PROJECT" > "$output.instance"
  jq -S -n --argjson revisions "$revisions" \
    --slurpfile project "$output.project" --slurpfile profile "$output.profile" \
    --slurpfile instance "$output.instance" '
      def canonical: with_entries(
        if (.key as $key | $revisions | index($key)) != null then
          if (.value | test("^0:[a-f0-9]{64}:")) then .value |= sub("^0:[a-f0-9]{64}:"; "0:RELEASE:")
          else error("invalid disabled publication") end else . end);
      $instance[0].config as $config |
      if ($config["volatile.uuid"] | type != "string" or length == 0) then error("missing native UUID") else
      {project: $project[0].config, profile: {config: $profile[0].config, devices: $profile[0].devices},
       instance: {profiles: $instance[0].profiles, config: ($config | canonical),
         expanded_config: ($instance[0].expanded_config | canonical),
         devices: $instance[0].devices, expanded_devices: $instance[0].expanded_devices}}
      end' > "$output"
  chmod 0600 "$output" "$output.project" "$output.profile" "$output.instance"
}

assert_legacy_native_policy() {
  capture_legacy_native_policy "$STATE_ROOT/native-policy.current"
  cmp -s "$STATE_ROOT/native-policy.before" "$STATE_ROOT/native-policy.current" \
    || die 'compatible transition changed the exact legacy native policy or identity'
  assert_legacy_continuation
}

verify_legacy_rollback_refusal() {
  local path index=0 grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  local -a paths=("$V2_STATE_ROOT/history-checkpoint.json" "$V2_LEDGER" "$V2_JOURNAL"
    "$OPERATOR_HOME/.config/subyard/migrations/state.json" "$grant")
  capture_legacy_native_policy "$STATE_ROOT/native-policy.before"
  for path in "${paths[@]}"; do
    # Fresh v0.11.0 is V2: retain V1 evidence if present, never manufacture it.
    if [ "$index" = 3 ] && operator_env test ! -e "$path" && operator_env test ! -L "$path"; then
      printf 'absent\n' > "$STATE_ROOT/refusal-$index.mode"
    else
      operator_env stat -c '%a:%u:%g:%d:%i' "$path" > "$STATE_ROOT/refusal-$index.mode"
      operator_env cat "$path" > "$STATE_ROOT/refusal-$index.bytes"
    fi
    index=$((index + 1))
  done
  if operator_yard update --rollback --yes > "$STATE_ROOT/predecessor-rollback.log" 2>&1; then
    die 'incompatible published v0.11.0 rollback unexpectedly succeeded'
  fi
  grep -Fq 'code=rollback-incompatible' "$STATE_ROOT/predecessor-rollback.log" \
    || die 'published predecessor rollback failed for an unexpected reason'
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  index=0
  for path in "${paths[@]}"; do
    if [ "$index" = 3 ]; then
      assert_published_v1_history_unchanged
    else
      [ "$(operator_env stat -c '%a:%u:%g:%d:%i' "$path")" = "$(cat "$STATE_ROOT/refusal-$index.mode")" ] \
        && operator_env cat "$path" | cmp -s "$STATE_ROOT/refusal-$index.bytes" - \
        || die 'refused rollback changed protected history or grant'
    fi
    index=$((index + 1))
  done
  assert_legacy_native_policy
  ok 'incompatible published v0.11.0 rollback refused without mutation'
}

check_compatible_transition() {
  local version="$1" release_root="$2" target="${3#releases/}" active="${4#releases/}"
  operator_env env YARD_RELEASE_BASE_URL="file://$release_root" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$version" --check --json \
    > "$STATE_ROOT/compatible-check.json"
  jq -e --arg target "$target" --arg active "$active" '
    .outcome.target == $target and .outcome.active == $active and
    .outcome.status == "migration-required" and .outcome.code == "transition-required" and
    .outcome.reachedGoal == false and .assessment.changed == true and
    (.plan | type == "string" and startswith("plan-v1-")) and
    ((.blockers // []) | type == "array" and length == 0)
  ' "$STATE_ROOT/compatible-check.json" >/dev/null \
    || die 'public update inspection did not authorize the exact compatible target'
}

finish_candidate_flow() {
  local rollback_catalog rollback_ledger runtime="$OPERATOR_HOME/.subyard/runtime"
  load_release_targets
  verify_legacy_rollback_refusal
  info 'retaining the authentic published v0.21.0 runtime through verified public updates'
  check_compatible_transition "$COMPATIBLE_VERSION" "$COMPATIBLE_RELEASE_ROOT" \
    "$COMPATIBLE_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  operator_env env YARD_RELEASE_BASE_URL="file://$COMPATIBLE_RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$COMPATIBLE_VERSION" --yes
  assert_runtime_links "$COMPATIBLE_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  assert_runtime_state "$COMPATIBLE_VERSION" \
    "$runtime/$COMPATIBLE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" loaded candidate \
    "$COMPATIBLE_RELEASE_TARGET"
  assert_legacy_native_policy
  assert_published_v1_history_unchanged
  check_compatible_transition "$CANDIDATE_VERSION" "$RELEASE_ROOT" \
    "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  assert_candidate_state
  assert_legacy_native_policy
  rollback_catalog="$(operator_env jq -er '.catalogDigest | select(type == "string" and test("^[a-f0-9]{64}$"))' "$V2_JOURNAL")"
  rollback_ledger="$(operator_env sha256sum "$V2_LEDGER" | awk '{print $1}')"
  info 'rolling back to the exact compatible published v0.21.0 runtime'
  operator_yard update --rollback --yes
  assert_runtime_links "$COMPATIBLE_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  assert_runtime_state "$COMPATIBLE_VERSION" \
    "$runtime/$COMPATIBLE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" loaded candidate \
    "$COMPATIBLE_RELEASE_TARGET"
  assert_published_v1_history_unchanged
  assert_v2_transition activate-previous "$COMPATIBLE_RELEASE_TARGET" "$rollback_catalog"
  [ "$(operator_env sha256sum "$V2_LEDGER" | awk '{print $1}')" = "$rollback_ledger" ] \
    || die 'rollback changed the exact v2 migration ledger bytes'
  assert_legacy_native_policy
  check_compatible_transition "$CANDIDATE_VERSION" "$RELEASE_ROOT" \
    "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  assert_candidate_state
  assert_legacy_native_policy
  operator_yard init --yes
  assert_legacy_native_policy
  verify_legacy_full_removal_refusal
  assert_legacy_native_policy
  assert_published_v1_history_unchanged
  ok 'authentic compatible rollback and candidate roll-forward retained limited legacy authority'
}

verify_compatible_capabilities() {
  local response="$1"
  [ "$(wc -c < "$response")" -le 65536 ] \
    && jq -e 'keys == ["activationOwners", "activationReconciliationOwned", "schemaVersion"] and
      .activationReconciliationOwned == false and .schemaVersion == 2 and
      (.activationOwners | sort) == ["core-guest-runtime", "git-fetch-runtime", "github-runtime",
        "host-power", "key-tools-runtime", "openlit-runtime", "orca-runtime",
        "test-vm-broker", "test-yard-route-consumers"]' \
      "$response" >/dev/null \
    || die 'published compatible runtime lacks exact activation capabilities'
}
