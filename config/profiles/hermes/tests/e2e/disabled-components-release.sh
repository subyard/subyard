#!/usr/bin/env bash
# Installed-runtime disabled-component acceptance on one disposable worker, retained across one reboot.
set -Eeuo pipefail
if [ "${1:-}" = --help ]; then
  printf 'Usage: disabled-components-release.sh prepare|resume|clean NUMERIC_TOKEN\nRun through dev/e2e/disabled-components-release.sh.\n'
  exit 0
fi
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
[[ "${2:-}" =~ ^[0-9]{1,10}$ ]] || { printf 'expected fresh numeric token\n' >&2; exit 2; }
case "${1:-}" in prepare|resume|clean|evidence) ;; *) printf 'expected prepare, resume, clean or evidence\n' >&2; exit 2 ;; esac
cd "$ROOT"
# Reuse the disposable operator, root runtime snapshot and marker-guarded cleanup.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
CONFIG_HOME="$OPERATOR_HOME/.config/subyard"
GIT_ROOT="/tmp/subyard-disabled-components-$TOKEN"
EXTRA_PROJECT_ARMED="$STATE_ROOT/.hermes-project-armed"
FAULT_MARKER=/var/tmp/subyard-disabled-components-hook
CANDIDATE_INSTALLED=0

guest_yard() {
  local name="$1" project=subyard instance=yard
  shift
  [ "$name" = default ] || { project="subyard-$name"; instance="yard-$name"; }
  incus exec "$instance" --project "$project" -- "$@"
}

fixture_cleanup() {
  local rc=$? failed=0
  if [ "$rc" = 0 ] && [ "$PRESERVE_FIXTURE" = 1 ]; then cleanup; fi
  trap - EXIT INT TERM ERR
  set +e
  if [ "$CLEANUP_ARMED" = 1 ]; then
    assert_state_root
    if [ "$rc" != 0 ]; then
      if [ "$CANDIDATE_INSTALLED" = 1 ] && [ ! -e "$STATE_ROOT/upgrade-capture-status.txt" ] \
        && [ ! -L "$STATE_ROOT/upgrade-capture-status.txt" ]; then
        capture_upgrade_failure "$rc" fixture-prepare \
          || printf 'fixture preparation evidence capture failed (exit %s)\n' "$?" >&2
      fi
      install -d -m 0755 "$ROOT/.build/disabled-components-release"
      # Preserve the bounded synthetic diagnostics before guarded fixture deletion.
      sudo -n tar --ignore-failed-read -C "$STATE_ROOT" -czf \
        "$ROOT/.build/disabled-components-release/failure.tar.gz" -- \
        default-inventory.json hermes-inventory.json \
        all-local-plan.json all-local-plan.txt all-local-apply.txt all-local-repeat.txt failure-plan.json selected-hook-failure.log \
        recovery-plan.json recovery-init.txt recovery-status.json recovery-repeat.txt \
        activation-journal.before activation-ledger.before postboot-ready.json upgrade-capture-status.txt \
        upgrade-inspection.json upgrade-journal.json upgrade-ledger.json upgrade-identities.txt \
        installer.log installer-ready.json installer-config-status.txt \
        inventory-boundaries.jsonl selected-hook-proof.json hermes-dashboard-native.jsonl dashboard-disabled.txt \
        deselect-assets.before deselect-assets.failed deselect-assets.repaired deselect-assets.after deselect-plan.json \
        deselect-failure.log deselect-recovery-plan.json deselect-repair.log deselect-init.txt deselect-status.json \
        deselect-repeat.txt 2>/dev/null || failed=1
    fi
    # shellcheck source=config/profiles/hermes/tests/e2e/disabled-release-resource.sh
    . "$ROOT/config/profiles/hermes/tests/e2e/disabled-release-resource.sh"
    hermes_disabled_resource_cleanup || failed=1
    if [ -f "$EXTRA_PROJECT_ARMED" ]; then
      (PROJECT=subyard-hermes INSTANCE=yard-hermes; delete_owned_project) || failed=1
    fi
    if [ -e "$GIT_ROOT" ] || [ -L "$GIT_ROOT" ]; then
      if [ -d "$GIT_ROOT" ] && [ ! -L "$GIT_ROOT" ] \
        && [ "$(sudo -n cat "$GIT_ROOT/.marker")" = "$MARKER" ]; then
        sudo -n find "$GIT_ROOT" -xdev -depth -delete || failed=1
      else failed=1; fi
    fi
  fi
  [ "$failed" = 0 ] || rc=3
  (exit "$rc")
  cleanup
}
trap fixture_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'printf "disabled-components-release: line %s failed (exit %s)\n" "$LINENO" "$?" >&2' ERR

# shellcheck source=dev/e2e/published-baseline.sh
. "$ROOT/dev/e2e/published-baseline.sh"

seed_sources() {
  local host_id host_base
  [ ! -e "$GIT_ROOT" ] && [ ! -L "$GIT_ROOT" ] || die 'Git fixture path already exists'
  sudo -n install -d -o "$OPERATOR" -g "$OPERATOR" -m 0700 "$GIT_ROOT"
  operator_env bash -c 'printf "%s\n" "$2" > "$1/.marker"; chmod 0600 "$1/.marker"' _ "$GIT_ROOT" "$MARKER"
  host_id="disabled-components-$TOKEN"
  operator_env bash -c '
    set -eu
    root=$1; host=$2
    install -d -m 0700 "$root/source/hosts/$host/yards/default" "$root/source/hosts/$host/yards/hermes"
    printf "{\"schemaVersion\":1}\n" > "$root/source/subyard-config.json"
    printf "CODING_TOOL_INTEGRATIONS=\047codex opencode\047\nSSH_PORT=2228\n" > "$root/source/hosts/$host/config.env"
    printf "CODING_TOOL_INTEGRATIONS=codex\nSSH_PORT=2222\n" > "$root/source/hosts/$host/yards/default/config.env"
    printf "SSH_PORT=2233\n" > "$root/source/hosts/$host/yards/hermes/config.env"
    find "$root/source" -type f -exec chmod 0600 {} +
    git -C "$root/source" init -q
    git -C "$root/source" add -- .
    git -C "$root/source" -c user.name="Subyard Test" -c user.email=test@invalid commit -qm "Seed isolated selection and ports"
    # Remove the helper fixture legacy empty alias before connecting canonical Git settings.
    sed -i "/^AGENTS=/d" "$3"
  ' _ "$GIT_ROOT" "$host_id" "$CONFIG_HOME/config.env"
  for name in default hermes; do
    host_base="$OPERATOR_HOME/host-data"
    [ "$name" = default ] || host_base="$OPERATOR_HOME/host-data-$name"
    operator_env install -d -m 0700 "$CONFIG_HOME/yards/$name"
    operator_env bash -c 'printf "HOST_BASE=%s\nRESTRICTED_DISK_PATHS=%s\nFORWARD_SSH_AGENT=0\n" "$2" "$2" > "$1"; chmod 0600 "$1"' _ \
      "$CONFIG_HOME/yards/$name/config.env" "$host_base"
  done
  # Hermes is an environment profile, not a YARD_TEMPLATE. Keep its installed
  # preset guards, with selection and ports inherited from the isolated Git source.
  operator_env bash -c '
    set -eu
    sed -e "/^CODING_TOOL_INTEGRATIONS=/d" -e "/^SSH_PORT=/d" -e "/^FORWARD_SSH_AGENT=/d" "$2" >> "$1"
  ' _ "$CONFIG_HOME/yards/hermes/config.env" "$OPERATOR_HOME/.subyard/runtime/current/config/profiles/hermes/yard.env"
  operator_yard config sync connect "$GIT_ROOT/source" --host-id "$host_id" --checkout "$GIT_ROOT/checkout" --yes
  operator_env sha256sum "$GIT_ROOT/source/hosts/$host_id/config.env" \
    "$GIT_ROOT/source/hosts/$host_id/yards/default/config.env" \
    "$GIT_ROOT/source/hosts/$host_id/yards/hermes/config.env" > "$STATE_ROOT/source.before"
}

create_yards() {
  local name project
  for name in default hermes; do
    project=subyard
    [ "$name" = default ] || project=subyard-hermes
    ! incus project show "$project" >/dev/null 2>&1 || die 'fixture project already exists'
    [ "$name" != default ] || printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
    [ "$name" != hermes ] || printf '%s\n' "$MARKER" > "$EXTRA_PROJECT_ARMED"
    incus project create "$project" -c features.images=false -c user.p0-power-systemd="$MARKER" >/dev/null
    operator_yard -Y "$name" init --yes
    operator_yard -Y "$name" start --yes
  done
}

assert_disabled() {
  local name="$1"
  operator_yard -Y "$name" integration status opencode --json > "$STATE_ROOT/$name-status.json"
  jq -e '.observed_scope == "yard" and (.selection.effective | index("opencode") == null) and
    any(.inventory[]; .id == "opencode" and .effective == false and .observed == "not-inspected")' \
    "$STATE_ROOT/$name-status.json" >/dev/null || die 'disabled inventory contradicts selection'
}

capture_upgrade_failure() {
  local original_rc="$1" origin="${2:-installer}" capture_rc=0 file
  case "$origin" in installer|fixture-prepare) ;; *) return 2 ;; esac
  for file in upgrade-capture-status.txt upgrade-inspection.json upgrade-journal.json upgrade-ledger.json upgrade-identities.txt; do
    install -m 0600 /dev/null "$STATE_ROOT/$file" || return 1
  done
  printf 'failure_origin=%s\nfailure_exit_code=%s\n' "$origin" "$original_rc" > "$STATE_ROOT/upgrade-capture-status.txt"
  [ "$origin" != installer ] || printf 'installer_exit_code=%s\n' "$original_rc" >> "$STATE_ROOT/upgrade-capture-status.txt"
  (
    export -f operator_uid operator_env operator_yard
    export OPERATOR OPERATOR_HOME
    timeout --signal=KILL 45 bash -c '
      set -u
      capture_root=$1; journal_path=$2; ledger_path=$3
      failed=0
      timeout --signal=KILL 30 bash -c "operator_yard migrate --check --json" 2>/dev/null \
        | jq -e "select(type == \"object\" and .schemaVersion == 1 and (.outcome | type == \"object\")) |
          {decisions: [.decisions[]? | {resource,scope,decision}], blockers: [.blockers[]? | {code,resource}],
           outcome: (.outcome | {status,reachedGoal,active,previous,target,code,transaction})}" \
        > "$capture_root/upgrade-inspection.json" 2>/dev/null
      codes=("${PIPESTATUS[@]}")
      printf "inspection_exit_code=%s filter_exit_code=%s\n" "${codes[0]}" "${codes[1]}" >> "$capture_root/upgrade-capture-status.txt"
      [ "${codes[0]}" = 0 ] || [ "${codes[0]}" = 1 ] || failed=1
      [ "${codes[1]}" = 0 ] || failed=1
      operator_env jq -e "select(type == \"object\" and (.transaction | type == \"string\") and (.checkpoint | type == \"string\") and (.steps | type == \"array\")) |
        {schemaVersion,transaction,checkpoint,releases,artifactDigest,registryDigest,catalogDigest,observationScope,intentDigest,
         steps: [.steps[] | {id,migration,resource,decision,checkpoint,expectedFingerprint,desiredFingerprint,
                            observedFingerprint: .evidence.observedFingerprint}]}" "$journal_path" \
        > "$capture_root/upgrade-journal.json" 2>/dev/null
      rc=$?; printf "journal_exit_code=%s\n" "$rc" >> "$capture_root/upgrade-capture-status.txt"; [ "$rc" = 0 ] || failed=1
      operator_env jq -e "select(type == \"object\" and (.schemaVersion == 2) and (.domains | type == \"object\")) |
        {schemaVersion,domains: (.domains | with_entries(.value |= {epoch,applied}))}" "$ledger_path" \
        > "$capture_root/upgrade-ledger.json" 2>/dev/null
      rc=$?; printf "ledger_exit_code=%s\n" "$rc" >> "$capture_root/upgrade-capture-status.txt"; [ "$rc" = 0 ] || failed=1
      operator_env sha256sum "$journal_path" "$ledger_path" >> "$capture_root/upgrade-identities.txt" 2>/dev/null
      rc=$?; printf "identity_exit_code=%s\n" "$rc" >> "$capture_root/upgrade-capture-status.txt"; [ "$rc" = 0 ] || failed=1
      for link in current previous; do
        value="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/$link" 2>/dev/null)"; rc=$?
        printf "%s_link=%s\n" "$link" "$value" >> "$capture_root/upgrade-identities.txt"
        printf "%s_link_exit_code=%s\n" "$link" "$rc" >> "$capture_root/upgrade-capture-status.txt"; [ "$rc" = 0 ] || failed=1
      done
      exit "$failed"
    ' _ "$STATE_ROOT" "$V2_JOURNAL" "$V2_LEDGER"
  ) || capture_rc=$?
  printf 'capture_exit_code=%s\n' "$capture_rc" >> "$STATE_ROOT/upgrade-capture-status.txt"
  return "$capture_rc"
}

install_candidate_with_evidence() {
  local installer_rc=0 capture_rc=0
  install -m 0600 /dev/null "$STATE_ROOT/installer.log"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes \
    > "$STATE_ROOT/installer.log" 2>&1 \
    || installer_rc=$?
  cat "$STATE_ROOT/installer.log"
  if [ "$installer_rc" != 0 ]; then
    capture_upgrade_failure "$installer_rc" || capture_rc=$?
    printf 'candidate installer failed: installer_exit_code=%s capture_exit_code=%s\n' "$installer_rc" "$capture_rc" >&2
  else
    CANDIDATE_INSTALLED=1
  fi
  return "$installer_rc"
}

assert_one_run_ready() {
  grep -Fq 'Release is active; additional activation repair requires a fresh plan.' "$STATE_ROOT/installer.log" \
    || die 'installer did not assess the follow-up activation repair'
  grep -Fq 'Current release repaired successfully' "$STATE_ROOT/installer.log" \
    || die 'installer did not complete the follow-up activation repair'
  # The repair observes installed configuration and explains each yard's own installed selection.
  grep -Fq 'yard default: requested coding integrations [], effective [], from installed configuration (applied record)' "$STATE_ROOT/installer.log" \
    && grep -Fq 'yard hermes: requested coding integrations [codex], effective [codex], from installed configuration (applied record)' "$STATE_ROOT/installer.log" \
    || die 'activation repair plan misstated per-yard installed selection provenance'
  # Inspect the installed release without another update or release lookup.
  operator_yard migrate --check --json > "$STATE_ROOT/installer-ready.json"
  jq -e '.outcome.status == "ready" and .outcome.reachedGoal == true and
    .outcome.active == .outcome.target and (.blockers | length) == 0' \
    "$STATE_ROOT/installer-ready.json" >/dev/null || die 'one installer run did not reach release readiness'
  operator_yard config status --all-local > "$STATE_ROOT/installer-config-status.txt"
  for name in default hermes; do
    grep -Fxq "yard $name materialized-config: converged" "$STATE_ROOT/installer-config-status.txt" \
      || die "one installer run left yard $name unconverged"
  done
}

assert_inventory() {
  local name="$1" source_group="$2" requested="$3" boundary="$4"
  case "$boundary" in local-disabled|git-fallback|local-empty) ;; *) die 'unknown inventory proof boundary' ;; esac
  operator_yard -Y "$name" integration status --json > "$STATE_ROOT/$name-inventory.json"
  jq -e --arg group "$source_group" --argjson requested "$requested" '
    .observed_scope == "yard" and .selection.allows_coding_tools == true and .selection.requested == $requested and
    (.inventory | length) >= 6 and all(.inventory[]; .source_group == $group and (.reason | length) > 0)
  ' "$STATE_ROOT/$name-inventory.json" >/dev/null || die 'integration inventory source or requested selection differs'
  operator_yard -Y "$name" config show CODING_TOOL_INTEGRATIONS > "$STATE_ROOT/$name-config-show.txt"
  operator_yard -Y "$name" profile list --json > "$STATE_ROOT/$name-profiles.json"
  jq -e '(.profiles | length) > 1 and all(.profiles[]; has("eligible") and has("reason") and has("resources"))' \
    "$STATE_ROOT/$name-profiles.json" >/dev/null || die 'profile catalog lacks full selection diagnostics'
  [ -f "$STATE_ROOT/inventory-boundaries.jsonl" ] || install -m 0600 /dev/null "$STATE_ROOT/inventory-boundaries.jsonl"
  jq -c --arg yard "$name" --arg boundary "$boundary" '
    {yard:$yard,boundary:$boundary,observed_scope,
     selection:(.selection | {requested,effective,present,allows_coding_tools}),
     inventory:[.inventory[] | {id,requested,effective,reason,source_group,source:(.source | {scope:.Scope,role:.Role,status:.Status})}]}
  ' "$STATE_ROOT/$name-inventory.json" >> "$STATE_ROOT/inventory-boundaries.jsonl"
}

assert_candidate() {
  local target digest
  digest="$(jq -er .runtime_sha256 "$ROOT/.subyard-acceptance/candidate.json")"
  target="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  [ "$target" = "releases/$CANDIDATE_VERSION-${digest:0:12}" ] || die 'installed runtime differs from frozen package'
  operator_env bash -c 'cd "$1"; sha256sum -c runtime-files.sha256 >/dev/null' _ \
    "$OPERATOR_HOME/.subyard/runtime/$target"
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'installed CLI version differs'
  for name in default hermes; do
    expected=2222
    [ "$name" = default ] || expected=2233
    operator_yard -Y "$name" config show SSH_PORT > "$STATE_ROOT/$name-port.txt"
    grep -Fxq "effective: $expected" "$STATE_ROOT/$name-port.txt" || die 'persistent endpoint changed'
    if ! operator_env grep -Fxq "SSH_PORT='$expected'" "$CONFIG_HOME/yards/$name/config.env"; then
      operator_env grep -Fxq "SSH_PORT=$expected" "$CONFIG_HOME/yards/$name/config.env" || die 'legacy Git port was not pinned locally'
    fi
    project=subyard; instance=yard
    [ "$name" = default ] || { project=subyard-hermes; instance=yard-hermes; }
    incus query "/1.0/instances/$instance?project=$project" | jq -e --arg port ":$expected" '
      .devices | to_entries | any(.value.type == "proxy" and
      (.value.listen | endswith($port)) and (.value.connect | endswith(":22")))
    ' >/dev/null || die 'native SSH endpoint changed'
  done
}

exercise_recovery() {
  local rc=0 transaction current previous
  # Selection is init-owned on a ready release: the repair keeps the installed view and
  # the selected provisioner runs in the post-repair yard init, never as activation work.
  operator_yard -Y hermes config set CODING_TOOL_INTEGRATIONS 'codex claude' --scope yard --local --yes
  guest_yard hermes sh -euc 'test ! -e /usr/local/bin/curl; test ! -e /var/tmp/subyard-disabled-components-hook'
  cat > "$STATE_ROOT/native-curl.sh" <<'FAULT'
#!/bin/sh
# subyard-disabled-components-curl-v1
printf '%s\n' hook-failed > /var/tmp/subyard-disabled-components-hook
chmod 0600 /var/tmp/subyard-disabled-components-hook
exit 75
FAULT
  chmod 0755 "$STATE_ROOT/native-curl.sh"
  incus file push "$STATE_ROOT/native-curl.sh" yard-hermes/usr/local/bin/curl --project subyard-hermes
  guest_yard hermes sh -euc 'chown root:root /usr/local/bin/curl; chmod 0755 /usr/local/bin/curl; test "$(command -v curl)" = /usr/local/bin/curl'
  operator_yard migrate --check --json > "$STATE_ROOT/failure-plan.json"
  jq -e '.outcome.status == "ready" and (.blockers | length) == 0' "$STATE_ROOT/failure-plan.json" >/dev/null \
    || die 'init-owned selection change entered release readiness'
  sudo -n install -m 0600 "$V2_JOURNAL" "$ACTIVATION_JOURNAL_BASELINE"
  sudo -n install -m 0600 "$V2_LEDGER" "$ACTIVATION_LEDGER_BASELINE"
  transaction="$(sudo -n jq -er .transaction "$V2_JOURNAL")"
  current="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  previous="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")"
  operator_yard migrate --yes > "$STATE_ROOT/selected-hook-failure.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] || { cat "$STATE_ROOT/selected-hook-failure.log"; die 'native selected provision fault was not reached'; }
  guest_yard hermes grep -Fxq hook-failed "$FAULT_MARKER" || die 'selected provision did not reach the native fault'
  { grep -Fq 'release.init.hermes' "$STATE_ROOT/selected-hook-failure.log" &&
    grep -Fq 'pending yard init did not complete: yard hermes:' "$STATE_ROOT/selected-hook-failure.log" &&
    grep -Fq 'next: run yard -Y hermes init' "$STATE_ROOT/selected-hook-failure.log"; } \
    || { cat "$STATE_ROOT/selected-hook-failure.log"; die 'failed post-repair init did not name its yard init continuation'; }
  # The release repair stays complete; only ordinary init owns the remaining work.
  sudo -n jq -e --arg transaction "$transaction" '.checkpoint == "complete" and .transaction == $transaction and
    (.steps | length) == 0 and .releases.from == .releases.target' "$V2_JOURNAL" >/dev/null \
    || die 'failed post-repair init changed the release journal'
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER"
  [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$current" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")" = "$previous" ] \
    || die 'failed post-repair init changed release links'
  operator_yard migrate --check --json > "$STATE_ROOT/recovery-plan.json"
  jq -e '.outcome.status == "ready" and (.blockers | length) == 0' "$STATE_ROOT/recovery-plan.json" >/dev/null \
    || die 'failed post-repair init undid release readiness'
  install -m 0600 /dev/null "$STATE_ROOT/selected-hook-proof.json"
  jq -n --argjson native_exit "$rc" --arg transaction "$transaction" --slurpfile check "$STATE_ROOT/recovery-plan.json" '
    {operation:"migrate",native_exit_code:$native_exit,resource:"hermes pending yard init provision",
     injected_marker:"hook-failed",checkpoint:"complete",transaction:$transaction,
     continuation:"yard -Y hermes init",outcome:($check[0].outcome | {status,reachedGoal,active,previous,target,code})}
  ' > "$STATE_ROOT/selected-hook-proof.json"
  guest_yard hermes sh -euc '
    test "$(head -n 2 /usr/local/bin/curl | tail -n 1)" = "# subyard-disabled-components-curl-v1"
    rm /usr/local/bin/curl /var/tmp/subyard-disabled-components-hook
  '
  operator_yard -Y hermes init --yes > "$STATE_ROOT/recovery-init.txt" 2>&1 \
    || { cat "$STATE_ROOT/recovery-init.txt"; die 'yard init did not complete after the fault was removed'; }
  operator_yard -Y hermes integration status --json > "$STATE_ROOT/recovery-status.json"
  jq -e '(.selection.effective | sort) == ["claude","codex"]' "$STATE_ROOT/recovery-status.json" >/dev/null \
    || die 'recovered yard init did not apply the selected integrations'
  sudo -n jq -e --arg transaction "$transaction" '.checkpoint == "complete" and .transaction == $transaction' \
    "$V2_JOURNAL" >/dev/null || die 'ordinary yard init changed the release journal'
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER"
  operator_yard migrate --yes > "$STATE_ROOT/recovery-repeat.txt" 2>&1 \
    || { cat "$STATE_ROOT/recovery-repeat.txt"; die 'repeated repair after yard init failed'; }
  ! grep -Fq 'release.init.' "$STATE_ROOT/recovery-repeat.txt" || die 'yard init left the selection pending'
  assert_disabled default
  assert_disabled hermes
}

# Deselection fingerprint of the installed claude assets in the hermes yard: its
# structured settings document and any recorded persist links.
claude_assets() {
  guest_yard hermes sh -euc '
    home="$(getent passwd dev | cut -d: -f6)"
    for path in "$home/.claude/settings.json" "$home/.claude/projects" "$home/.claude-memory"; do
      if [ -L "$path" ]; then printf "%s link %s %s\n" "$path" "$(stat -c %u:%g "$path")" "$(readlink "$path")"
      elif [ -f "$path" ]; then printf "%s file %s %s\n" "$path" "$(stat -c %u:%g:%a "$path")" "$(sha256sum < "$path" | cut -d " " -f 1)"
      elif [ -e "$path" ]; then printf "%s other\n" "$path"
      else printf "%s absent\n" "$path"; fi
    done
  '
}

# A failed post-repair init that deselects an installed integration leaves its owned
# assets: the release stays ready, repair restores nothing, and the next init retires them.
exercise_failed_deselect() {
  local rc=0 transaction
  claude_assets > "$STATE_ROOT/deselect-assets.before"
  # Persist links exist only where the session mount does; the settings document always does.
  grep -q '/\.claude/settings\.json file ' "$STATE_ROOT/deselect-assets.before" \
    || { cat "$STATE_ROOT/deselect-assets.before"; die 'installed claude settings are missing before deselection'; }
  operator_yard -Y hermes config set CODING_TOOL_INTEGRATIONS codex --scope yard --local --yes
  # The remaining selected provisioner fails after inventory apply, before the hooks.
  cat > "$STATE_ROOT/deselect-curl.sh" <<'FAULT'
#!/bin/sh
# subyard-disabled-components-curl-v1
printf '%s\n' hook-failed > /var/tmp/subyard-disabled-components-hook
chmod 0600 /var/tmp/subyard-disabled-components-hook
exit 75
FAULT
  chmod 0755 "$STATE_ROOT/deselect-curl.sh"
  guest_yard hermes sh -euc 'test ! -e /usr/local/bin/curl; test ! -e /var/tmp/subyard-disabled-components-hook'
  incus file push "$STATE_ROOT/deselect-curl.sh" yard-hermes/usr/local/bin/curl --project subyard-hermes
  guest_yard hermes sh -euc 'chown root:root /usr/local/bin/curl; chmod 0755 /usr/local/bin/curl; test "$(command -v curl)" = /usr/local/bin/curl'
  operator_yard migrate --check --json > "$STATE_ROOT/deselect-plan.json"
  jq -e '.outcome.status == "ready" and (.blockers | length) == 0' "$STATE_ROOT/deselect-plan.json" >/dev/null \
    || die 'init-owned deselection entered release readiness'
  transaction="$(sudo -n jq -er .transaction "$V2_JOURNAL")"
  operator_yard migrate --yes > "$STATE_ROOT/deselect-failure.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] || { cat "$STATE_ROOT/deselect-failure.log"; die 'native deselecting init fault was not reached'; }
  guest_yard hermes grep -Fxq hook-failed "$FAULT_MARKER" || die 'deselecting init did not reach the native fault'
  grep -Fq 'pending yard init did not complete: yard hermes:' "$STATE_ROOT/deselect-failure.log" \
    || { cat "$STATE_ROOT/deselect-failure.log"; die 'failed deselecting init did not name its yard init continuation'; }
  claude_assets > "$STATE_ROOT/deselect-assets.failed"
  cmp -s "$STATE_ROOT/deselect-assets.before" "$STATE_ROOT/deselect-assets.failed" \
    || { diff "$STATE_ROOT/deselect-assets.before" "$STATE_ROOT/deselect-assets.failed" || true; die 'failed deselecting init retired installed assets before its hooks'; }
  operator_yard migrate --check --json > "$STATE_ROOT/deselect-recovery-plan.json"
  jq -e '.outcome.status == "ready" and (.blockers | length) == 0' "$STATE_ROOT/deselect-recovery-plan.json" >/dev/null \
    || die 'failed deselecting init undid release readiness'
  # The repair itself is complete; with the fault in place only the pending init fails again.
  rc=0
  operator_yard migrate --yes > "$STATE_ROOT/deselect-repair.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] && grep -Fq 'pending yard init did not complete: yard hermes:' "$STATE_ROOT/deselect-repair.log" \
    || { cat "$STATE_ROOT/deselect-repair.log"; die 'repeated repair did not leave only the pending deselecting init'; }
  sudo -n jq -e --arg transaction "$transaction" '.checkpoint == "complete" and .transaction == $transaction' \
    "$V2_JOURNAL" >/dev/null || die 'failed deselecting init changed the release journal'
  claude_assets > "$STATE_ROOT/deselect-assets.repaired"
  cmp -s "$STATE_ROOT/deselect-assets.before" "$STATE_ROOT/deselect-assets.repaired" \
    || { diff "$STATE_ROOT/deselect-assets.before" "$STATE_ROOT/deselect-assets.repaired" || true; die 'release repair changed assets awaiting the deselecting init'; }
  guest_yard hermes sh -euc '
    test "$(head -n 2 /usr/local/bin/curl | tail -n 1)" = "# subyard-disabled-components-curl-v1"
    rm /usr/local/bin/curl /var/tmp/subyard-disabled-components-hook
  '
  operator_yard -Y hermes init --yes > "$STATE_ROOT/deselect-init.txt" 2>&1 \
    || { cat "$STATE_ROOT/deselect-init.txt"; die 'deselecting yard init did not complete after the fault was removed'; }
  operator_yard -Y hermes integration status --json > "$STATE_ROOT/deselect-status.json"
  jq -e '.selection.effective == ["codex"]' "$STATE_ROOT/deselect-status.json" >/dev/null \
    || die 'recovered yard init did not apply the deselection'
  claude_assets > "$STATE_ROOT/deselect-assets.after"
  ! grep -q ' link ' "$STATE_ROOT/deselect-assets.after" \
    && ! cmp -s "$STATE_ROOT/deselect-assets.before" "$STATE_ROOT/deselect-assets.after" \
    || { cat "$STATE_ROOT/deselect-assets.after"; die 'successful yard init did not retire the deselected assets'; }
  operator_yard migrate --yes > "$STATE_ROOT/deselect-repeat.txt" 2>&1 \
    || { cat "$STATE_ROOT/deselect-repeat.txt"; die 'repeated repair after deselecting yard init failed'; }
  ! grep -Fq 'release.init.' "$STATE_ROOT/deselect-repeat.txt" || die 'yard init left the deselection pending'
  assert_disabled hermes
}

prepare_acceptance() {
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  release_candidate_prepare "$ROOT" >/dev/null
  CANDIDATE_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  [ "$CANDIDATE_VERSION" != "$BASELINE_VERSION" ] || die 'candidate must differ from predecessor'
  ensure_platform
  prepare_fixture
  install -d -m 0755 "$RELEASE_ROOT"
  cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
  chmod -R a+rX "$RELEASE_ROOT"
  install_baseline
  seed_sources
  create_yards
  operator_yard -Y hermes integration disable opencode --yes
  operator_yard -Y default config set CODING_TOOL_INTEGRATIONS '' --scope yard --local --yes
  operator_yard migrate --yes
  install_candidate_with_evidence
  assert_one_run_ready
  assert_candidate
  assert_disabled default
  assert_disabled hermes
  assert_inventory default 'local overrides' '[]' local-disabled
  assert_inventory hermes 'local overrides' '["codex"]' local-disabled
  # Supported resolver views prove absence restores host Git and explicit empty masks it.
  operator_yard -Y hermes config unset CODING_TOOL_INTEGRATIONS --scope yard --local --yes
  assert_inventory hermes 'Git settings' '["codex","opencode"]' git-fallback
  operator_yard -Y hermes config set CODING_TOOL_INTEGRATIONS '' --scope yard --local --yes
  assert_inventory hermes 'local overrides' '[]' local-empty
  # Selection is init-owned: a ready release stays ready while its repair keeps the
  # installed view, and the repair plan lists each yard's pending selection for yard init.
  operator_yard migrate --check --json > "$STATE_ROOT/all-local-plan.json"
  jq -e '.outcome.status == "ready" and (.blockers | length) == 0 and
    ([.. | strings] | all(contains("local overrides") | not))' "$STATE_ROOT/all-local-plan.json" >/dev/null \
    || die 'local selection change entered the installed release readiness plan'
  rc=0
  operator_yard migrate </dev/null > "$STATE_ROOT/all-local-plan.txt" 2>&1 || rc=$?
  { [ "$rc" != 0 ] && grep -Fq 'confirmation required' "$STATE_ROOT/all-local-plan.txt" &&
    grep -Fxq '  yard default: integration settings current' "$STATE_ROOT/all-local-plan.txt" &&
    grep -Fxq '  yard hermes:' "$STATE_ROOT/all-local-plan.txt" &&
    grep -Fxq '    CODING_TOOL_INTEGRATIONS  codex -> (empty)' "$STATE_ROOT/all-local-plan.txt" &&
    grep -Fq 'release.init.hermes' "$STATE_ROOT/all-local-plan.txt" &&
    ! grep -Fq 'release.init.default' "$STATE_ROOT/all-local-plan.txt"; } \
    || { cat "$STATE_ROOT/all-local-plan.txt"; die 'all-local repair plan omitted distinct pending selection per yard'; }
  operator_yard migrate --yes > "$STATE_ROOT/all-local-apply.txt" 2>&1 \
    || { cat "$STATE_ROOT/all-local-apply.txt"; die 'all-local repair did not apply pending yard init'; }
  cat "$STATE_ROOT/all-local-apply.txt"
  grep -Fxq '  yard -Y hermes init' "$STATE_ROOT/all-local-apply.txt" || die 'all-local repair did not run hermes yard init'
  operator_yard migrate --yes > "$STATE_ROOT/all-local-repeat.txt" 2>&1 \
    || { cat "$STATE_ROOT/all-local-repeat.txt"; die 'repeated all-local repair failed'; }
  ! grep -Fq 'release.init.' "$STATE_ROOT/all-local-repeat.txt" || die 'yard init left the local selection pending'
  assert_disabled default
  assert_disabled hermes
  operator_yard -Y hermes integration enable opencode --yes
  guest_yard hermes sh -euc 'command -v opencode >/dev/null'
  operator_yard -Y hermes integration disable opencode --yes
  assert_disabled hermes
  # shellcheck source=config/profiles/hermes/tests/e2e/disabled-release-resource.sh
  . "$ROOT/config/profiles/hermes/tests/e2e/disabled-release-resource.sh"
  hermes_disabled_resource_prepare
  exercise_recovery
  exercise_failed_deselect
  operator_yard migrate --yes
  hermes_disabled_resource_assert preboot
  assert_candidate
  operator_env sha256sum -c "$STATE_ROOT/source.before" >/dev/null || die 'migration changed external Git source'
  write_fixture_value "$BOOT_ID_STATE" "$(cat /proc/sys/kernel/random/boot_id)"
  write_fixture_value "$PHASE_STATE" disabled-ready
  PRESERVE_FIXTURE=1
  printf 'ok: predecessor upgrade and pending-init recovery; reboot pending\n'
}

resume_acceptance() {
  assert_state_root
  CLEANUP_ARMED=1
  assert_fixture_phase disabled-ready
  [ "$(cat /proc/sys/kernel/random/boot_id)" != "$(read_fixture_value "$BOOT_ID_STATE")" ] || die 'reboot was not observed'
  CANDIDATE_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  for name in default hermes; do
    for _ in $(seq 1 120); do
      [ "$(incus list "$( [ "$name" = default ] && printf yard || printf yard-hermes )" \
        --project "$( [ "$name" = default ] && printf subyard || printf subyard-hermes )" --format csv -c s)" = RUNNING ] && break
      sleep 1
    done
    assert_disabled "$name"
  done
  # shellcheck source=config/profiles/hermes/tests/e2e/disabled-release-resource.sh
  . "$ROOT/config/profiles/hermes/tests/e2e/disabled-release-resource.sh"
  hermes_disabled_resource_assert postboot
  operator_yard migrate --check --json > "$STATE_ROOT/postboot-ready.json"
  jq -e '.outcome.code == "ready" and (.blockers | length) == 0' "$STATE_ROOT/postboot-ready.json" >/dev/null
  operator_yard migrate --yes
  assert_candidate
  assert_disabled default
  assert_disabled hermes
  hermes_disabled_resource_assert repeat
  PRESERVE_FIXTURE=1
  printf 'ok: installed final candidate retained disabled integration and Hermes route through reboot/repeat\n'
}

for command in sudo systemctl timeout cmp curl jq sha256sum python3 git; do
  command -v "$command" >/dev/null || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on disposable worker'
case "$MODE" in
  prepare) prepare_acceptance ;;
  resume) resume_acceptance ;;
  evidence)
    assert_state_root
    # Synthetic resolver diagnostics only; never auth or config state.
    sudo -n tar -C "$STATE_ROOT" -czf - -- \
      default-inventory.json hermes-inventory.json default-profiles.json hermes-profiles.json \
      default-port.txt hermes-port.txt all-local-plan.json failure-plan.json \
      recovery-plan.json recovery-status.json activation-journal.before activation-ledger.before postboot-ready.json \
      installer.log installer-ready.json installer-config-status.txt \
      inventory-boundaries.jsonl selected-hook-proof.json hermes-dashboard-native.jsonl dashboard-disabled.txt
    PRESERVE_FIXTURE=1
    ;;
  clean) if [ -e "$STATE_ROOT" ]; then assert_state_root; CLEANUP_ARMED=1; fi ;;
esac
