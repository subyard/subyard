#!/usr/bin/env bash
# Installed-runtime Paseo acceptance on one disposable worker, retained across one reboot.
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
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
EXTRA_PROJECT_ARMED="$STATE_ROOT/.hermes-project-armed"
BASELINE_VERSION=0.18.6
BASELINE_INSTALLER_SHA256=a78c10910c0886a8d01c0a2e77718b21ce58e45fa7f0de6064bc1f351d56028b
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
        default-inventory.json hermes-inventory.json missing-receipt.json stale-receipt.json \
        cleanup-plan.txt collision-plan.txt cleanup-repeat.txt \
        all-local-plan.json failure-plan.json selected-hook-failure.log reduction-plan.txt \
        reduction-decline.txt recovery-plan.json recovery-decline.txt recovery-receipt.json \
        activation-journal.before activation-ledger.before postboot-ready.json baseline-readiness.txt \
        default-disabled-native.txt hermes-disabled-native.txt upgrade-capture-status.txt \
        upgrade-inspection.json upgrade-journal.json upgrade-ledger.json upgrade-identities.txt \
        inventory-boundaries.jsonl selected-hook-proof.json hermes-dashboard-native.jsonl dashboard-disabled.txt 2>/dev/null || failed=1
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

ensure_platform() {
  p0_capacity_prepare_root
  p0_capacity_prepare_platform_root
  if ! incus info >/dev/null 2>&1 || ! incus storage show default --project default >/dev/null 2>&1; then
    (
      # shellcheck source=tests/helpers/test-context.sh
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$P0_CAPACITY_STATE_ROOT/bootstrap"
      export SUBYARD_USER SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
      SUBYARD_USER="$(id -un)"
      export STORAGE_PATH="$P0_CAPACITY_PLATFORM_ROOT/incus/incus/storage"
      set -a
      # shellcheck source=config/host.env
      . "$ROOT/config/host.env"
      set +a
      timeout --signal=TERM --kill-after=10 900 bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
    )
  fi
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  if ! incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1; then
    timeout --signal=TERM --kill-after=10 900 sudo -n /usr/bin/incus image copy \
      images:debian/13/cloud local: --alias "$BASE_IMAGE" --project default >/dev/null
  fi
}

install_baseline() {
  # Exact published predecessor pins shared with reliable-migrations-recovery.sh.
  local archive pin name baseline="$STATE_ROOT/baseline"
  case "$(uname -m)" in
    x86_64) archive="subyard-$BASELINE_VERSION-linux-amd64.tar.gz"; pin=458950eccd3831df8d4ed059ccc51a5dece6f8a85aedaae38f863b7252f635f2 ;;
    aarch64) archive="subyard-$BASELINE_VERSION-linux-arm64.tar.gz"; pin=a3b52c39edfc4930f0e0ab4c697d60db6b39a00b022d344053bc4f4131fca4fa ;;
    *) die 'unsupported predecessor architecture' ;;
  esac
  install -d -m 0755 "$baseline"
  for name in "$archive" "$archive.sha256" "$archive.manifest.json" "$archive.provenance.json" \
    subyard-install-runtime-release.sh subyard-install-runtime-release.sh.sha256 subyard-install.sh; do
    curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
      "https://github.com/Subyard/Subyard/releases/download/v$BASELINE_VERSION/$name" -o "$baseline/$name"
  done
  [ "$(sha256sum "$baseline/$archive" | awk '{print $1}')" = "$pin" ] || die 'predecessor runtime checksum mismatch'
  [ "$(sha256sum "$baseline/subyard-install.sh" | awk '{print $1}')" = "$BASELINE_INSTALLER_SHA256" ] \
    || die 'predecessor installer checksum mismatch'
  chmod -R a+rX "$baseline"
  chmod 0755 "$baseline/subyard-install.sh"
  operator_env env YARD_RELEASE_BASE_URL="file://$baseline" "$baseline/subyard-install.sh" --version "$BASELINE_VERSION" --yes
}

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
    printf "CODING_TOOL_INTEGRATIONS=\047codex paseo\047\nSSH_PORT=2228\n" > "$root/source/hosts/$host/config.env"
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

stage_native_bundle() {
  local arch asset checksum
  arch=amd64
  [ "$(uname -m)" != aarch64 ] || arch=arm64
  asset="paseo-headless-0.2.1-linux-$arch.tar.gz"
  for name in "$asset" "$asset.sha256"; do
    curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
      "https://github.com/Subyard/Subyard/releases/download/v$BASELINE_VERSION/$name" -o "$STATE_ROOT/$name"
  done
  checksum="$(awk -v name="$asset" 'NF == 2 && $2 == name {print $1}' "$STATE_ROOT/$asset.sha256")"
  [[ "$checksum" =~ ^[0-9a-f]{64}$ ]] \
    && [ "$(sha256sum "$STATE_ROOT/$asset" | awk '{print $1}')" = "$checksum" ] || die 'native Paseo bundle checksum mismatch'
  [ "$arch" != amd64 ] || [ "$checksum" = fbaf0c162b8e1ce811e0703f7293c25c1785c0dff5b7648c0a58fcf702421781 ] \
    || die 'published native Paseo artifact differs from preflight pin'
  printf 'evidence: native_paseo_bundle_sha256=%s published_baseline=%s\n' "$checksum" "$BASELINE_VERSION"
  guest_yard hermes sh -euc 'test ! -e /usr/local/bin/bash; test ! -e /var/tmp/subyard-disabled-paseo.tar.gz'
  incus file push "$STATE_ROOT/$asset" yard-hermes/var/tmp/subyard-disabled-paseo.tar.gz --project subyard-hermes
  guest_yard hermes chmod 0644 /var/tmp/subyard-disabled-paseo.tar.gz
  cat > "$STATE_ROOT/native-bash.sh" <<'SHIM'
#!/bin/sh
# subyard-disabled-components-bash-v1
export PASEO_BUNDLE_FILE=/var/tmp/subyard-disabled-paseo.tar.gz
exec /bin/bash "$@"
SHIM
  chmod 0755 "$STATE_ROOT/native-bash.sh"
  incus file push "$STATE_ROOT/native-bash.sh" yard-hermes/usr/local/bin/bash --project subyard-hermes
  guest_yard hermes sh -euc 'chown root:root /usr/local/bin/bash /var/tmp/subyard-disabled-paseo.tar.gz; chmod 0755 /usr/local/bin/bash; test "$(command -v bash)" = /usr/local/bin/bash'
  # Its unchanged provisioner verifies the full manifest/dependencies/files before install.
}

create_yards() {
  local name project
  for name in default hermes; do
    project=subyard
    [ "$name" = default ] || project=subyard-hermes
    ! incus project show "$project" >/dev/null 2>&1 || die 'fixture project already exists'
    [ "$name" != default ] || printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
    [ "$name" != hermes ] || printf '%s\n' "$MARKER" > "$EXTRA_PROJECT_ARMED"
    incus project create "$project" -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
    operator_yard -Y "$name" init --yes
    operator_yard -Y "$name" start --yes
  done
}

wait_for_baseline_paseo() {
  local rc=0 probe_code
  install -m 0600 /dev/null "$STATE_ROOT/baseline-readiness.txt"
  # Incus agent access can precede the enabled unit's ExecStartPost completion.
  # The native deadline and each probe are bounded; this only observes the unit.
  probe_code='
    set -euo pipefail
    clock() { local uptime; read -r uptime _ < /proc/uptime; printf "%s\n" "${uptime%%.*}"; }
    deadline=$(($(clock) + 90))
    while true; do
      remaining=$((deadline - $(clock)))
      [ "$remaining" -gt 0 ] || { printf "outcome=readiness-deadline\n"; exit 1; }
      budget=$remaining; [ "$budget" -le 5 ] || budget=5
      snapshot=""; rc=0
      snapshot=$(timeout --foreground --signal=KILL "$budget" systemctl show paseo.service \
        -p LoadState -p ActiveState -p SubState -p UnitFileState -p Result -p MainPID \
        -p ExecMainCode -p ExecMainStatus -p NRestarts -p Job -p InvocationID \
        -p ActiveEnterTimestampMonotonic -p InactiveExitTimestampMonotonic \
        -p ExecMainStartTimestampMonotonic 2>/dev/null) || rc=$?
      printf "probe=unit-properties monotonic_seconds=%s exit_code=%s\n%s\n" "$(clock)" "$rc" "$snapshot"
      [ "$rc" = 0 ] && grep -q "^LoadState=loaded$" <<< "$snapshot" \
        && grep -q "^SubState=." <<< "$snapshot" && grep -q "^UnitFileState=enabled$" <<< "$snapshot" \
        && grep -q "^Result=success$" <<< "$snapshot" \
        || { printf "outcome=unit-unavailable-disabled-or-failed\n"; exit 1; }
      case "$(sed -n "s/^ActiveState=//p" <<< "$snapshot")" in
        active)
          grep -q "^SubState=running$" <<< "$snapshot" \
            && grep -Eq "^MainPID=[1-9][0-9]*$" <<< "$snapshot" \
            || { printf "outcome=running-process-unavailable\n"; exit 1; }
          for predicate in is-active is-enabled; do
            remaining=$((deadline - $(clock)))
            [ "$remaining" -gt 0 ] || { printf "outcome=readiness-deadline\n"; exit 1; }
            budget=$remaining; [ "$budget" -le 5 ] || budget=5
            rc=0
            timeout --foreground --signal=KILL "$budget" systemctl "$predicate" --quiet paseo.service 2>/dev/null || rc=$?
            printf "probe=%s monotonic_seconds=%s exit_code=%s\n" "$predicate" "$(clock)" "$rc"
            [ "$rc" = 0 ] || exit 1
          done
          printf "outcome=active-and-enabled\n"; exit 0 ;;
        activating|inactive) ;;
        *) printf "outcome=unexpected-unit-state\n"; exit 1 ;;
      esac
      [ "$(clock)" -lt "$deadline" ] || { printf "outcome=readiness-deadline\n"; exit 1; }
      sleep 1
    done
  '
  (
    export -f incus guest_yard
    INCUS_COMMAND_TIMEOUT=90 INCUS_KILL_AFTER_SECONDS=1 \
      timeout --foreground --signal=KILL 90 bash -c '
        guest_yard hermes timeout --foreground --signal=KILL 90 bash -c "$1"
      ' _ "$probe_code"
  ) > "$STATE_ROOT/baseline-readiness.txt" 2>/dev/null || rc=$?
  printf 'probe=readiness-transport exit_code=%s\n' "$rc" >> "$STATE_ROOT/baseline-readiness.txt"
  [ "$rc" = 0 ] || die 'baseline Paseo did not become active and enabled within the passive readiness boundary'
}

capture_native_off() {
  local name="$1" boundary="${2:-repeat}" rc=0
  [ -f "$STATE_ROOT/$name-disabled-native.txt" ] \
    || install -m 0600 /dev/null "$STATE_ROOT/$name-disabled-native.txt"
  printf 'boundary=%s\n' "$boundary" >> "$STATE_ROOT/$name-disabled-native.txt"
  (
    export -f incus guest_yard
    INCUS_COMMAND_TIMEOUT=15 INCUS_KILL_AFTER_SECONDS=1 \
      timeout --foreground --signal=KILL 15 bash -c 'guest_yard "$@"' _ "$name" sh -euc '
    snapshot="$(systemctl show paseo.service -p LoadState -p ActiveState -p SubState -p UnitFileState -p MainPID -p ControlPID 2>/dev/null)" \
      || { printf "disabled Paseo unit state could not be read\n" >&2; exit 1; }
    printf "%s\n" "$snapshot" | grep -q "^UnitFileState=" \
      && printf "%s\n" "$snapshot" | grep -q "^MainPID=0$" \
      && printf "%s\n" "$snapshot" | grep -q "^ControlPID=0$" \
      || { printf "disabled Paseo unit state is incomplete\n" >&2; exit 1; }
    load="$(printf "%s\n" "$snapshot" | sed -n "s/^LoadState=//p")"
    active="$(printf "%s\n" "$snapshot" | sed -n "s/^ActiveState=//p")"
    sub="$(printf "%s\n" "$snapshot" | sed -n "s/^SubState=//p")"
    enabled="$(printf "%s\n" "$snapshot" | sed -n "s/^UnitFileState=//p")"
    case "$load:$active:$sub:$enabled" in
      loaded:inactive:dead:disabled|not-found:inactive:dead:) ;;
      *) printf "Paseo unit is unavailable or retains activity/autostart\n" >&2; exit 1 ;;
    esac
    printf "%s\n" "$snapshot"
    ! systemctl is-active --quiet paseo.service
    ! systemctl is-enabled --quiet paseo.service
    test ! -L /etc/systemd/system/multi-user.target.wants/paseo.service
  '
  ) >> "$STATE_ROOT/$name-disabled-native.txt" 2>/dev/null || rc=$?
  printf 'probe=disabled-native exit_code=%s\n' "$rc" >> "$STATE_ROOT/$name-disabled-native.txt"
  [ "$rc" = 0 ] || die 'disabled Paseo native state was unavailable or retained activity/autostart'
}

assert_disabled() {
  local name="$1"
  capture_native_off "$name"
  operator_yard -Y "$name" integration status paseo --json > "$STATE_ROOT/$name-status.json"
  jq -e '.observed_scope == "yard" and (.selection.effective | index("paseo") == null) and
    any(.inventory[]; .id == "paseo" and .effective == false and .observed == "not-inspected")' \
    "$STATE_ROOT/$name-status.json" >/dev/null || die 'disabled inventory contradicts native state'
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
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes \
    || installer_rc=$?
  if [ "$installer_rc" != 0 ]; then
    capture_upgrade_failure "$installer_rc" || capture_rc=$?
    printf 'candidate installer failed: installer_exit_code=%s capture_exit_code=%s\n' "$installer_rc" "$capture_rc" >&2
  else
    CANDIDATE_INSTALLED=1
  fi
  return "$installer_rc"
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

exercise_legacy_cleanup() {
  local backup_before backup_collision_before
  # Native legacy/unowned state is separate from deselection; inspect before explicit cleanup.
  guest_yard hermes sh -euc '
    cp /var/lib/subyard/paseo-ownership /var/tmp/subyard-disabled-receipt.before
    chmod 0600 /var/tmp/subyard-disabled-receipt.before
    rm /var/lib/subyard/paseo-ownership
  '
  operator_yard -Y hermes integration status paseo --json > "$STATE_ROOT/missing-receipt.json"
  jq -e '.observed == "conflict" and (.detail | contains("ownership"))' "$STATE_ROOT/missing-receipt.json" >/dev/null
  guest_yard hermes sh -euc '
    install -m 0600 /var/tmp/subyard-disabled-receipt.before /var/lib/subyard/paseo-ownership
    printf "# stale native fixture\n" >> /etc/systemd/system/paseo.service
  '
  operator_yard -Y hermes integration status paseo --json > "$STATE_ROOT/stale-receipt.json"
  jq -e '.observed == "conflict" and (.detail | contains("ownership"))' "$STATE_ROOT/stale-receipt.json" >/dev/null
  operator_yard -Y hermes integration cleanup paseo --check > "$STATE_ROOT/cleanup-plan.txt"
  operator_yard -Y hermes integration cleanup paseo --yes
  guest_yard hermes sh -euc '
    test ! -e /etc/systemd/system/paseo.service
    test -f /etc/systemd/system/paseo.service.subyard-retired
    install -m 0644 /etc/systemd/system/paseo.service.subyard-retired /etc/systemd/system/paseo.service
    systemctl daemon-reload
  '
  backup_before="$(guest_yard hermes sha256sum /etc/systemd/system/paseo.service.subyard-retired)"
  operator_yard -Y hermes integration cleanup paseo --check > "$STATE_ROOT/collision-plan.txt"
  grep -Fq 'paseo.service.subyard-retired.1' "$STATE_ROOT/collision-plan.txt" || die 'cleanup did not preserve earlier backup'
  operator_yard -Y hermes integration cleanup paseo --yes
  [ "$(guest_yard hermes sha256sum /etc/systemd/system/paseo.service.subyard-retired)" = "$backup_before" ] \
    || die 'cleanup overwrote earlier retirement backup'
  guest_yard hermes test -f /etc/systemd/system/paseo.service.subyard-retired.1
  backup_collision_before="$(guest_yard hermes sha256sum /etc/systemd/system/paseo.service.subyard-retired.1)"
  operator_yard -Y hermes integration cleanup paseo --yes > "$STATE_ROOT/cleanup-repeat.txt"
  grep -Fxq '  [skip] integration.cleanup: subyard-hermes/yard-hermes:integration paseo' "$STATE_ROOT/cleanup-repeat.txt" \
    || die 'repeat cleanup did not report the converged native target'
  [ "$(guest_yard hermes sha256sum /etc/systemd/system/paseo.service.subyard-retired)" = "$backup_before" ] \
    || die 'repeat cleanup changed the earlier retirement backup'
  [ "$(guest_yard hermes sha256sum /etc/systemd/system/paseo.service.subyard-retired.1)" = "$backup_collision_before" ] \
    || die 'repeat cleanup changed the collision retirement backup'
  guest_yard hermes sh -euc '
    test ! -e /etc/systemd/system/paseo.service
    test ! -L /etc/systemd/system/paseo.service
    test ! -e /etc/systemd/system/paseo.service.subyard-retired.2
    test ! -L /etc/systemd/system/paseo.service.subyard-retired.2
  '
  # Remove only stale fixture receipt, not auth/history/binaries, before explicit re-enable.
  guest_yard hermes sh -euc 'rm /var/lib/subyard/paseo-ownership; rm /var/tmp/subyard-disabled-receipt.before'
}

exercise_recovery() {
  local rc=0 predecessor replacement current previous
  operator_yard -Y hermes config set CODING_TOOL_INTEGRATIONS 'codex paseo' --scope yard --local --yes
  guest_yard hermes sh -euc 'test ! -e /usr/local/bin/systemctl; test ! -e /var/tmp/subyard-disabled-components-hook'
  cat > "$STATE_ROOT/native-systemctl.sh" <<'FAULT'
#!/bin/sh
# subyard-disabled-components-systemctl-v1
set -eu
case " $* " in
  *' paseo.service '*)
    case "${1:-}" in enable|restart)
      printf '%s\n' hook-failed > /var/tmp/subyard-disabled-components-hook
      chmod 0600 /var/tmp/subyard-disabled-components-hook
      exit 75
    esac
    ;;
esac
exec /usr/bin/systemctl "$@"
FAULT
  chmod 0755 "$STATE_ROOT/native-systemctl.sh"
  incus file push "$STATE_ROOT/native-systemctl.sh" yard-hermes/usr/local/bin/systemctl --project subyard-hermes
  guest_yard hermes sh -euc 'chown root:root /usr/local/bin/systemctl; chmod 0755 /usr/local/bin/systemctl; test "$(command -v systemctl)" = /usr/local/bin/systemctl'
  operator_yard migrate --check --json > "$STATE_ROOT/failure-plan.json"
  operator_yard migrate --yes > "$STATE_ROOT/selected-hook-failure.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] || die 'native selected provision fault was not reached'
  guest_yard hermes grep -Fxq hook-failed "$FAULT_MARKER" || die 'selected provision did not reach the native fault'
  sudo -n jq -e '.checkpoint == "reconciling" and (.steps | length) == 0 and .releases.from == .releases.target' \
    "$V2_JOURNAL" >/dev/null || die 'fault did not create original activation-only reconciliation'
  install -m 0600 /dev/null "$STATE_ROOT/selected-hook-proof.json"
  (
    set +e
    export -f operator_uid operator_env operator_yard
    export OPERATOR OPERATOR_HOME
    timeout --signal=KILL 30 bash -c 'operator_yard migrate --check --json' 2>/dev/null \
      | jq -e 'select(type == "object" and .schemaVersion == 1 and (.outcome | type == "object")) |
        {decisions:[.decisions[]? | {resource,scope,decision}],blockers:[.blockers[]? | {code,resource}],
         outcome:(.outcome | {status,reachedGoal,active,previous,target,code,transaction})}' 2>/dev/null
    codes=("${PIPESTATUS[@]}")
    printf '{"inspection_exit_code":%s,"filter_exit_code":%s}\n' "${codes[0]}" "${codes[1]}"
  ) | jq -s --argjson native_exit "$rc" '
    (map(select(has("inspection_exit_code")))[0]) as $probe |
    {operation:"migrate",native_exit_code:$native_exit,resource:"paseo.service",injected_marker:"hook-failed",
     checkpoint:"reconciling",migration_steps:0,same_release:true,
     diagnostic:(if ($probe.inspection_exit_code == 0 or $probe.inspection_exit_code == 1) and $probe.filter_exit_code == 0
       then (map(select(has("outcome")))[0] // null) else null end),probe:$probe}
  ' > "$STATE_ROOT/selected-hook-proof.json"
  sudo -n install -m 0600 "$V2_JOURNAL" "$ACTIVATION_JOURNAL_BASELINE"
  sudo -n install -m 0600 "$V2_LEDGER" "$ACTIVATION_LEDGER_BASELINE"
  predecessor="$(sudo -n jq -er .transaction "$V2_JOURNAL")"
  current="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  previous="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")"
  operator_yard -Y hermes config reduce-selection CODING_TOOL_INTEGRATIONS '' --scope yard --local --check \
    > "$STATE_ROOT/reduction-plan.txt"
  rc=0
  operator_yard -Y hermes config reduce-selection CODING_TOOL_INTEGRATIONS '' --scope yard --local \
    </dev/null > "$STATE_ROOT/reduction-decline.txt" 2>&1 || rc=$?
  [ "$rc" != 0 ] && grep -Fq 'confirmation required' "$STATE_ROOT/reduction-decline.txt" \
    || die 'source-only reduction reused activation consent'
  operator_yard -Y hermes config reduce-selection CODING_TOOL_INTEGRATIONS '' --scope yard --local --yes
  sudo -n cmp "$ACTIVATION_JOURNAL_BASELINE" "$V2_JOURNAL"
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER"
  [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$current" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")" = "$previous" ] \
    || die 'source-only reduction changed release links'
  guest_yard hermes sh -euc '
    test "$(head -n 2 /usr/local/bin/systemctl | tail -n 1)" = "# subyard-disabled-components-systemctl-v1"
    test "$(head -n 2 /usr/local/bin/bash | tail -n 1)" = "# subyard-disabled-components-bash-v1"
    rm /usr/local/bin/systemctl /usr/local/bin/bash /var/tmp/subyard-disabled-components-hook /var/tmp/subyard-disabled-paseo.tar.gz
  '
  operator_yard migrate --check --json > "$STATE_ROOT/recovery-plan.json"
  jq -e --arg transaction "$predecessor" '.outcome.code == "recovery-pending" and .outcome.transaction == $transaction and (.blockers | length) == 0' \
    "$STATE_ROOT/recovery-plan.json" >/dev/null
  rc=0
  operator_yard migrate </dev/null > "$STATE_ROOT/recovery-decline.txt" 2>&1 || rc=$?
  [ "$rc" != 0 ] && grep -Fq 'confirmation required' "$STATE_ROOT/recovery-decline.txt" \
    || die 'fresh recovery reused source-reduction consent'
  sudo -n cmp "$ACTIVATION_JOURNAL_BASELINE" "$V2_JOURNAL"
  operator_yard migrate --yes
  replacement="$(sudo -n jq -er .transaction "$V2_JOURNAL")"
  [ "$replacement" != "$predecessor" ] || die 'recovery reused interrupted transaction'
  sudo -n jq -e '.checkpoint == "complete" and (.steps | length) == 0' "$V2_JOURNAL" >/dev/null
  sudo -n jq -e --arg transaction "$predecessor" --slurpfile original "$ACTIVATION_JOURNAL_BASELINE" '
    .schemaVersion == 2 and .contract == "activation-only-replacement-v2" and
    .predecessor == $original[0] and .replacement.transaction == $transaction and (.nativePlans | length) > 0
  ' "$CONFIG_HOME/release-transition/recovery/v2/transactions/$replacement.json" >/dev/null
  sudo -n install -m 0600 "$CONFIG_HOME/release-transition/recovery/v2/transactions/$replacement.json" "$STATE_ROOT/recovery-receipt.json"
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER"
  assert_disabled default
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
  wait_for_baseline_paseo
  operator_yard -Y hermes integration disable paseo --yes
  capture_native_off hermes baseline-disable
  operator_yard -Y default config set CODING_TOOL_INTEGRATIONS '' --scope yard --local --yes
  operator_yard migrate --yes
  install_candidate_with_evidence
  assert_candidate
  assert_disabled default
  assert_disabled hermes
  assert_inventory default 'local overrides' '[]' local-disabled
  assert_inventory hermes 'local overrides' '["codex"]' local-disabled
  # Supported resolver views prove absence restores host Git and explicit empty masks it.
  operator_yard -Y hermes config unset CODING_TOOL_INTEGRATIONS --scope yard --local --yes
  assert_inventory hermes 'Git settings' '["codex","paseo"]' git-fallback
  operator_yard -Y hermes config set CODING_TOOL_INTEGRATIONS '' --scope yard --local --yes
  assert_inventory hermes 'local overrides' '[]' local-empty
  operator_yard migrate --check --json > "$STATE_ROOT/all-local-plan.json"
  jq -e '[.. | strings] | any(contains("yard default:") and contains("local overrides")) and
    any(contains("yard hermes:") and contains("local overrides"))' "$STATE_ROOT/all-local-plan.json" >/dev/null \
    || die 'all-local activation plan omitted distinct selection provenance'
  operator_yard migrate --yes
  operator_yard migrate --yes
  assert_disabled default
  assert_disabled hermes
  stage_native_bundle
  operator_yard -Y hermes integration enable paseo --yes
  guest_yard hermes systemctl is-active --quiet paseo.service
  guest_yard hermes systemctl is-enabled --quiet paseo.service
  printf 'ok: unchanged candidate native Paseo provision validated the published bundle and enabled its service\n'
  operator_yard -Y hermes integration disable paseo --yes
  assert_disabled hermes
  exercise_legacy_cleanup
  # shellcheck source=config/profiles/hermes/tests/e2e/disabled-release-resource.sh
  . "$ROOT/config/profiles/hermes/tests/e2e/disabled-release-resource.sh"
  hermes_disabled_resource_prepare
  exercise_recovery
  operator_yard migrate --yes
  hermes_disabled_resource_assert preboot
  assert_candidate
  operator_env sha256sum -c "$STATE_ROOT/source.before" >/dev/null || die 'migration changed external Git source'
  write_fixture_value "$BOOT_ID_STATE" "$(cat /proc/sys/kernel/random/boot_id)"
  write_fixture_value "$PHASE_STATE" disabled-ready
  PRESERVE_FIXTURE=1
  printf 'ok: predecessor upgrade, native cleanup and fresh activation recovery; reboot pending\n'
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
  printf 'ok: installed final candidate retained disabled Paseo and Hermes route through reboot/repeat\n'
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
    # Synthetic resolver diagnostics and allowlisted unit readiness; never pairing/auth/config state.
    sudo -n tar -C "$STATE_ROOT" -czf - -- \
      default-inventory.json hermes-inventory.json default-profiles.json hermes-profiles.json \
      default-port.txt hermes-port.txt missing-receipt.json stale-receipt.json \
      cleanup-plan.txt collision-plan.txt cleanup-repeat.txt all-local-plan.json failure-plan.json \
      reduction-plan.txt reduction-decline.txt recovery-plan.json recovery-decline.txt \
      recovery-receipt.json activation-journal.before activation-ledger.before postboot-ready.json baseline-readiness.txt \
      default-disabled-native.txt hermes-disabled-native.txt \
      inventory-boundaries.jsonl selected-hook-proof.json hermes-dashboard-native.jsonl dashboard-disabled.txt
    PRESERVE_FIXTURE=1
    ;;
  clean) if [ -e "$STATE_ROOT" ]; then assert_state_root; CLEANUP_ARMED=1; fi ;;
esac
