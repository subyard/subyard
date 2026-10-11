#!/usr/bin/env bash
# Owning native test-yard source repair assertions. Sourced by the singleton
# reliable-migrations-recovery fixture; never acquire or build an inner VM.

source_repair_snapshot_host_sink() {
  local path name unit state
  install -d -m 0700 "$BEFORE_ROOT/sink"
  for path in /usr/local/libexec/subyard/test-vms-host-sink \
    /etc/systemd/system/subyard-test-vms-host-sink.service \
    /etc/systemd/system/subyard-test-vms-host-sink.timer; do
    name="$(basename "$path")"
    if sudo -n test -e "$path" || sudo -n test -L "$path"; then
      sudo -n test -f "$path" && sudo -n test ! -L "$path" \
        && [ "$(sudo -n stat -c %u:%g "$path")" = 0:0 ] || die 'unsafe existing host sink input'
      sudo -n install -m "$(sudo -n stat -c %a "$path")" "$path" "$BEFORE_ROOT/sink/$name"
      printf 'present\n' > "$BEFORE_ROOT/sink/$name.state"
    else
      printf 'absent\n' > "$BEFORE_ROOT/sink/$name.state"
    fi
  done
  for unit in subyard-test-vms-host-sink.service subyard-test-vms-host-sink.timer; do
    state="$(sudo -n systemctl is-enabled "$unit" 2>/dev/null || true)"
    case "$state" in enabled|disabled|static|not-found) ;; *) die 'unsupported initial host sink enablement' ;; esac
    printf '%s\n' "$state" > "$BEFORE_ROOT/sink/$unit.enabled"
    state="$(sudo -n systemctl show "$unit" --property=ActiveState --value)"
    case "$state" in active|inactive) ;; *) die 'unsupported initial host sink activity' ;; esac
    printf '%s\n' "$state" > "$BEFORE_ROOT/sink/$unit.active"
  done
  printf '%s\n' "$MARKER" > "$BEFORE_ROOT/sink/.complete"
}

source_repair_restore_host_sink() {
  local path name unit state
  [ -f "$BEFORE_ROOT/sink/.complete" ] || return 0
  [ "$(cat "$BEFORE_ROOT/sink/.complete")" = "$MARKER" ] || return 1
  sudo -n systemctl disable --now subyard-test-vms-host-sink.timer >/dev/null 2>&1 || true
  sudo -n systemctl stop subyard-test-vms-host-sink.service >/dev/null 2>&1 || true
  for path in /usr/local/libexec/subyard/test-vms-host-sink \
    /etc/systemd/system/subyard-test-vms-host-sink.service \
    /etc/systemd/system/subyard-test-vms-host-sink.timer; do
    name="$(basename "$path")"
    case "$(cat "$BEFORE_ROOT/sink/$name.state")" in
      present) sudo -n install -D -o root -g root -m "$(stat -c %a "$BEFORE_ROOT/sink/$name")" "$BEFORE_ROOT/sink/$name" "$path" || return 1 ;;
      absent)
        if sudo -n test -e "$path" || sudo -n test -L "$path"; then
          sudo -n test -f "$path" && sudo -n test ! -L "$path" || return 1
          sudo -n find "$path" -delete || return 1
        fi ;;
      *) return 1 ;;
    esac
  done
  sudo -n systemctl daemon-reload || return 1
  for unit in subyard-test-vms-host-sink.service subyard-test-vms-host-sink.timer; do
    state="$(cat "$BEFORE_ROOT/sink/$unit.enabled")"
    case "$state" in
      enabled) sudo -n systemctl enable "$unit" >/dev/null || return 1 ;;
      disabled) sudo -n systemctl disable "$unit" >/dev/null || return 1 ;;
      static|not-found) ;;
      *) return 1 ;;
    esac
    [ "$(cat "$BEFORE_ROOT/sink/$unit.active")" != active ] || sudo -n systemctl start "$unit" || return 1
    [ "$(sudo -n systemctl is-enabled "$unit" 2>/dev/null || true)" = "$state" ] || return 1
    [ "$(sudo -n systemctl show "$unit" --property=ActiveState --value)" = "$(cat "$BEFORE_ROOT/sink/$unit.active")" ] || return 1
  done
}

source_repair_assert_native_routes() {
  local yard project instance devices
  for yard in default named; do
    project=subyard; instance=yard
    [ "$yard" = default ] || { project="subyard-$yard"; instance="yard-$yard"; }
    for devices in devices expanded_devices; do
      incus query "/1.0/instances/$instance?project=$project" \
        | jq -e --arg field "$devices" --arg source "$OPERATOR_HOME/.subyard/e2e/routes" \
          '.[$field]["subyard-e2e-routes"] | .type == "disk" and .source == $source and .path == "/var/lib/subyard/e2e-routes" and .readonly == "true"' >/dev/null \
        || die 'actual route consumer disk is missing or not readonly'
    done
    incus exec "$instance" --project "$project" -- test -s /var/lib/subyard/e2e-routes/test-yard/current/route.tsv
    incus exec "$instance" --project "$project" -- test -s /var/lib/subyard/e2e-routes/test-yard/current/known_hosts
  done
  # Ordinary init provisions broker metadata only. No cold base or held slot is permitted.
  operator_yard -Y test-yard test-vms status --json > "$STATE_ROOT/source-pool-status.json"
  jq -e '.pool.slots | length == 1 and all(.[]; .state == "available" and (.lease_id // "") == "")' "$STATE_ROOT/source-pool-status.json" >/dev/null \
    || die 'fixture acquired or held a nested VM slot'
  incus exec yard-test-yard --project subyard-test-yard -- incus list --format json | jq -e 'length == 0' >/dev/null
  incus exec yard-test-yard --project subyard-test-yard -- incus image list --format json | jq -e 'length == 0' >/dev/null
}

source_repair_history_identity() {
  operator_env python3 - "$CONFIG_HOME/release-transition" <<'PY'
import hashlib, os, pathlib, sys
root = pathlib.Path(sys.argv[1])
digest = hashlib.sha256()
for path in sorted(root.rglob('*')):
    info = path.lstat()
    digest.update(str(path.relative_to(root)).encode() + b'\0')
    digest.update(str((info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid, info.st_nlink)).encode())
    if path.is_file():
        digest.update(path.read_bytes())
print(digest.hexdigest())
PY
}

source_repair_assessment_diagnostic() {
  local assessment="$1" rc="$2" summary
  summary="$(jq -ce '
    def known($values): . as $value | if $values | index($value) then $value else "unavailable" end;
    {status: (.outcome.status | known(["ready", "migration-required", "recovering", "operator-action-required"])),
     code: (.outcome.code | known(["dependency-unavailable", "plan-stale", "journal-invalid", "precondition-blocked", "transition-required", "recovery-pending"])),
     resource: (if any(.blockers[]?; .resource == "activation.test-yard-route-consumers") then "activation.test-yard-route-consumers" else "unavailable" end),
     count: ((.blockers // []) | length | if . <= 64 then . else 64 end)}
  ' "$assessment" 2>/dev/null)" || summary='{"status":"unavailable","code":"unavailable","resource":"unavailable","count":0}'
  printf 'source-repair assessment rc=%s %s\n' "$rc" "$summary" >&2
}

source_repair_apply_explicit_choice() {
  local before original mode rc=0
  before="$(source_repair_history_identity)"
  original="$(operator_env sha256sum "$CONFIG_HOME/yards/named/config.env" | awk '{print $1}')"
  mode="$(operator_env stat -c %a "$CONFIG_HOME/yards/named/config.env")"
  operator_env "$SOURCE_TOOLING/bin/yard" -Y named config repair-alias ENVIRONMENT_PROFILES '' --scope yard --local --check
  operator_env "$SOURCE_TOOLING/bin/yard" -Y named config repair-alias ENVIRONMENT_PROFILES '' --scope yard --local </dev/null > "$STATE_ROOT/source-decline.log" 2>&1 || rc=$?
  [ "$rc" = 1 ] && grep -Fq 'confirmation required' "$STATE_ROOT/source-decline.log" || die 'source repair reused release consent'
  [ "$(operator_env sha256sum "$CONFIG_HOME/yards/named/config.env" | awk '{print $1}')" = "$original" ] || die 'source preview or decline wrote configuration'
  operator_env "$SOURCE_TOOLING/bin/yard" -Y named config repair-alias ENVIRONMENT_PROFILES '' --scope yard --local --yes
  [ "$(source_repair_history_identity)" = "$before" ] || die 'source repair changed protected history identity'
  [ "$(operator_env stat -c %a "$CONFIG_HOME/yards/named/config.env")" = "$mode" ] || die 'source repair changed permissions'
  operator_env grep -Fxq 'SSH_HOST=fixture-host' "$CONFIG_HOME/yards/named/config.env"
  operator_env "$SOURCE_TOOLING/bin/yard" -Y named config validate --json
  operator_env "$SOURCE_TOOLING/bin/yard" -Y named config repair-alias ENVIRONMENT_PROFILES '' --scope yard --local --yes
  [ "$(source_repair_history_identity)" = "$before" ] || die 'source no-op changed protected history'
  [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$OLD_RELEASE_TARGET" ] && operator_env test ! -e "$OPERATOR_HOME/.subyard/runtime/previous" || die 'source repair changed runtime links'
}

run_source_repair_acceptance() {
  local staged rc=0 predecessor before source_reader reader_identity reader_digest
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  release_candidate_prepare "$ROOT" >/dev/null
  CANDIDATE_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  ensure_platform
  prepare_fixture
  source_repair_snapshot_host_sink
  install -d -m 0755 "$RELEASE_ROOT"
  cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
  chmod -R a+rX "$RELEASE_ROOT"
  if ! incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1; then
    timeout --signal=TERM --kill-after=10 900 sudo -n /usr/bin/incus image copy images:debian/13/cloud local: --alias "$BASE_IMAGE" --project default </dev/null >/dev/null
    # shellcheck disable=SC2034 # consumed by the caller's guarded cleanup
    CREATED_IMAGE="$(image_fingerprint)"
  fi
  download_baseline
  OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  operator_yard config set CODING_TOOL_INTEGRATIONS claude --scope host --local --yes
  seed_yard named 2233
  seed_yard stopped 2234
  seed_yard test-yard 2223
  operator_env bash -c 'printf "YARD_TEMPLATE=test-vms\nE2E_VM_SLOT_COUNT=1\n" >> "$1"' _ "$CONFIG_HOME/yards/test-yard/config.env"
  create_local_yard test-yard
  create_local_yard default
  create_local_yard named
  create_local_yard stopped
  operator_yard -Y stopped stop --yes
  operator_yard migrate --yes
  source_repair_assert_native_routes
  operator_env bash -c 'printf "YARD_PROFILES=\n" >> "$1"' _ "$CONFIG_HOME/yards/named/config.env"
  operator_yard -Y named config set ENVIRONMENT_PROFILES orca-runtime --scope yard --local --yes
  rc=0
  operator_yard -Y named config show >/dev/null 2>&1 || rc=$?
  [ "$rc" != 0 ] || die 'released writer did not reproduce the source alias conflict'
  before="$(source_repair_history_identity)"
  rc=0
  # The released caller reserves every non-ready completed inspection for its
  # exact original owner. Asking for the new version rejects before rendering it.
  operator_yard update --check --json > "$STATE_ROOT/source-old-blocked.json" 2> "$STATE_ROOT/source-old-blocked.err" || rc=$?
  if [ "$rc" != 0 ] || ! jq -e '.outcome.status == "operator-action-required" and
      any(.blockers[]?; .resource == "activation.test-yard-route-consumers" and .code == "dependency-unavailable")' \
      "$STATE_ROOT/source-old-blocked.json" >/dev/null; then
    source_repair_assessment_diagnostic "$STATE_ROOT/source-old-blocked.json" "$rc"
    die 'released assessment did not expose the owning route-consumer blocker'
  fi
  rc=0
  candidate_update --check --json > "$STATE_ROOT/source-old-new-target.json" 2> "$STATE_ROOT/source-old-new-target.err" || rc=$?
  if [ "$rc" != 1 ] || ! grep -Fq 'permits only exact target' "$STATE_ROOT/source-old-new-target.err"; then
    source_repair_assessment_diagnostic "$STATE_ROOT/source-old-new-target.json" "$rc"
    die 'released caller did not refuse a different target behind its original-owner blocker'
  fi
  [ "$(source_repair_history_identity)" = "$before" ] || die 'released inspection changed protected history'
  [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$OLD_RELEASE_TARGET" ] \
    && operator_env test ! -e "$OPERATOR_HOME/.subyard/runtime/previous" || die 'released inspection changed runtime links'
  staged="$(operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" bash "$RELEASE_ROOT/subyard-install.sh" --publish-only --version "$CANDIDATE_VERSION")"
  SOURCE_TOOLING="$(sed -n 's/^staged release=.* candidate=//p' <<< "$staged")"
  [ -n "$SOURCE_TOOLING" ] && operator_env test -x "$SOURCE_TOOLING/bin/yard" || die 'staging did not publish verified tooling'
  source_repair_apply_explicit_choice
  operator_yard migrate --yes
  source_repair_assert_native_routes

  # Authenticate the unfinished original-owner case through actual native apply.
  materialize_unit "$OLD_UNIT_FIXTURE" "$STATE_ROOT/source-drift.service"
  sudo -n install -m 0644 "$STATE_ROOT/source-drift.service" "$UNIT"
  sudo -n systemctl daemon-reload
  install_activation_reconcile_fault
  rc=0
  operator_yard migrate --yes > "$STATE_ROOT/source-interruption.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] && operator_env test -f "$ACTIVATION_FAULT_PROBE" || die 'original-owner activation interruption was not reached'
  operator_env find "$ACTIVATION_SYSTEMCTL_WRAPPER" -delete
  sudo -n jq -e '.checkpoint == "reconciling" and (.steps | length) == 0 and .sourceIngress == null' "$V2_JOURNAL" >/dev/null
  predecessor="$(sudo -n jq -er .transaction "$V2_JOURNAL")"
  # Owned registration changes the observed scope after authentic publication.
  seed_yard recovery-scope 2237
  # Owned literal source drift follows authentic publication; no journal edits.
  operator_env bash -c 'printf "YARD_PROFILES=orca-runtime\n" >> "$1"' _ "$CONFIG_HOME/yards/named/config.env"
  source_repair_apply_explicit_choice
  before="$(source_repair_history_identity)"
  rc=0
  operator_yard migrate </dev/null > "$STATE_ROOT/source-recovery-decline.log" 2>&1 || rc=$?
  [ "$rc" = 1 ] && grep -Fq 'confirmation required' "$STATE_ROOT/source-recovery-decline.log" || die 'original-owner recovery reused source repair consent'
  [ "$(source_repair_history_identity)" = "$before" ] || die 'declined recovery changed protected history'
  operator_yard migrate --yes
  sudo -n jq -e --arg predecessor "$predecessor" '.checkpoint == "complete" and .transaction != $predecessor' "$V2_JOURNAL" >/dev/null
  operator_yard migrate --check --json > "$STATE_ROOT/source-original-ready.json"
  jq -e '.outcome.status == "ready"' "$STATE_ROOT/source-original-ready.json" >/dev/null
  candidate_update --yes
  source_repair_assert_native_routes
  before="$(source_repair_history_identity)"
  candidate_update --yes
  [ "$(source_repair_history_identity)" = "$before" ] || die 'ready update repetition changed history'

  # Persistent external reader coverage; an environment-only AGENT override
  # cannot establish the persistent schema validation contract.
  source_reader="$STATE_ROOT/root-readable.json"
  printf '{"env":{}}\n' > "$STATE_ROOT/reader-source.json"
  sudo -n install -o root -g root -m 0644 "$STATE_ROOT/reader-source.json" "$source_reader"
  seed_yard reader 2238
  operator_env bash -c 'printf "AGENT_claude_CONFIG=\047%s\047\n" "$2" >> "$1"' _ "$CONFIG_HOME/yards/reader/config.env" "$source_reader"
  operator_yard -Y reader config show AGENT_claude_CONFIG > "$STATE_ROOT/reader-resolved.log"
  grep -Fq "$source_reader" "$STATE_ROOT/reader-resolved.log" || die 'persistent root-owned reader was not resolved'
  reader_identity="$(sudo -n stat -c %d:%i:%a:%u:%g:%h "$source_reader")"
  reader_digest="$(sudo -n sha256sum "$source_reader" | awk '{print $1}')"
  operator_yard -Y reader config set SSH_PORT 2239 --scope yard --local --yes
  operator_yard -Y reader config set SSH_PORT 2238 --scope yard --local --yes
  [ "$(sudo -n stat -c %d:%i:%a:%u:%g:%h "$source_reader")" = "$reader_identity" ] \
    && [ "$(sudo -n sha256sum "$source_reader" | awk '{print $1}')" = "$reader_digest" ] \
    || die 'authoring changed the readonly external dependency identity or bytes'
  ok 'actual 0.18.6 native source repair preserves history, requires original-owner consent, and converges real readonly route consumers'
}
