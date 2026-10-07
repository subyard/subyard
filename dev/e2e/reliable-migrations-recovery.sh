#!/usr/bin/env bash
# Targeted published-runtime/config/recovery acceptance on one disposable worker.
# Transport .subyard-acceptance/candidate.json and .subyard-acceptance/release
# in the checksum-pinned public source bundle consumed by dev/agent-e2e.sh.
# Example (TOKEN is a fresh numeric fixture ID; SLOT is an allocated broker slot):
# SUBYARD_E2E_CANDIDATE_BUNDLE=/path/to/public-candidate.tar.gz \
# SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> dev/agent-e2e.sh \
#   --slot SLOT --vm-count 1 --vm 1 --purpose reliable-migrations-recovery -- \
#   timeout --signal=TERM --kill-after=30 3600 \
#   bash dev/e2e/reliable-migrations-recovery.sh run TOKEN
set -Eeuo pipefail

if [ "${1:-}" = --help ]; then
  printf '%s\n' 'Usage: reliable-migrations-recovery.sh run NUMERIC_TOKEN' \
    'Run only through dev/agent-e2e.sh on an allocated disposable worker.' \
    'Requires a frozen .subyard-acceptance candidate in the transported public bundle.'
  exit 0
fi
[ "${1:-}" = run ] || { printf 'expected run NUMERIC_TOKEN\n' >&2; exit 2; }
[[ "${2:-}" =~ ^[0-9]{1,16}$ ]] || { printf 'expected a fresh numeric token (1-16 digits)\n' >&2; exit 2; }

# Helper-only source mode returns before the old fixture's execution dispatch.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$(dirname "${BASH_SOURCE[0]}")/power-reconciler-upgrade.sh"
OLD_VERSION=0.17.3
OLD_INSTALLER_SHA256=a78c10910c0886a8d01c0a2e77718b21ce58e45fa7f0de6064bc1f351d56028b
INSTALLER="$STATE_ROOT/subyard-install-baseline.sh"
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
CREATED_IMAGE=''
OWNED_EXTRA_YARDS=()
CONFIG_HOME="$OPERATOR_HOME/.config/subyard"

cleanup_extra_yard() {
  local PROJECT="subyard-$1" INSTANCE="yard-$1"
  delete_owned_project
}

image_fingerprint() {
  incus image list --project default --format json | jq -er --arg alias "$BASE_IMAGE" \
    '.[] | select(any(.aliases[]; .name == $alias)) | .fingerprint'
}

cleanup_recovery_fixture() {
  local rc=$? yard failed=0
  trap - EXIT INT TERM ERR
  set +e
  if [ "$CLEANUP_ARMED" = 1 ]; then
    assert_state_root
    for yard in "${OWNED_EXTRA_YARDS[@]}"; do
      cleanup_extra_yard "$yard" || failed=1
    done
    if [ -n "$CREATED_IMAGE" ]; then
      if [ "$(image_fingerprint)" = "$CREATED_IMAGE" ]; then
        incus image delete "$CREATED_IMAGE" --project default >/dev/null || failed=1
      else
        failed=1
      fi
    fi
  fi
  [ "$failed" = 0 ] || rc=3
  # cleanup captures its caller's status and exits with it after guarded cleanup.
  (exit "$rc")
  cleanup
}
trap cleanup_recovery_fixture EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Report only the source location and status, never shell arguments or values.
trap 'printf "reliable-migrations-recovery: line %s failed (exit %s)\n" "$LINENO" "$?" >&2' ERR

ensure_platform() {
  if ! incus info >/dev/null 2>&1 || ! incus storage show default --project default >/dev/null 2>&1; then
    info 'bootstrapping Incus through the product installer in the marked platform cache'
    p0_capacity_prepare_root
    p0_capacity_prepare_platform_root
    (
      local bootstrap="$P0_CAPACITY_STATE_ROOT/incus-bootstrap" rc=0
      p0_capacity_prepare_subtree "$bootstrap"
      trap 'rc=$?; p0_capacity_remove_subtree "$bootstrap" || rc=3; exit "$rc"' EXIT
      # shellcheck source=tests/helpers/test-context.sh
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$bootstrap"
      export SUBYARD_USER SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
      SUBYARD_USER="$(id -un)"
      export STORAGE_PATH="$P0_CAPACITY_PLATFORM_ROOT/incus/incus/storage"
      set -a
      # shellcheck source=config/host.env
      . "$ROOT/config/host.env"
      set +a
      bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
    )
  fi
  incus info >/dev/null
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
}

download_baseline() {
  local arch archive pin name baseline="$STATE_ROOT/baseline"
  case "$(uname -m)" in
    x86_64) arch=amd64; pin=3ea73ca51dae023600997a07bbfaa5df8be1f4c1c4f5c9ead1b261b5aec4363b ;;
    aarch64) arch=arm64; pin=dcc1ae42dc25760b9f4fd290c8aec23c09b729d4a04f780107022710f1247e79 ;;
    *) die 'unsupported published runtime architecture' ;;
  esac
  archive="subyard-$OLD_VERSION-linux-$arch.tar.gz"
  install -d -m 0755 "$baseline"
  for name in "$archive" "$archive.sha256" "$archive.manifest.json" "$archive.provenance.json" \
    subyard-install-runtime-release.sh subyard-install-runtime-release.sh.sha256 subyard-install.sh; do
    curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
      "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/$name" -o "$baseline/$name"
  done
  [ "$(sha256sum "$baseline/$archive" | awk '{print $1}')" = "$pin" ] || die 'published baseline runtime checksum mismatch'
  [ "$(sha256sum "$baseline/subyard-install.sh" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] || die 'published baseline installer checksum mismatch'
  install -m 0755 "$baseline/subyard-install.sh" "$INSTALLER"
  chmod -R a+rX "$baseline"
  operator_env env YARD_RELEASE_BASE_URL="file://$baseline" "$INSTALLER" --version "$OLD_VERSION" --yes
}

seed_yard() {
  local yard="$1" port="$2" temporary="$STATE_ROOT/$1.config.env"
  printf 'SSH_PORT=%s\n# Retained unrelated fixture setting\nSSH_HOST=fixture-host\n' "$port" > "$temporary"
  operator_env install -d -m 0700 "$CONFIG_HOME/yards/$yard"
  sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0600 "$temporary" "$CONFIG_HOME/yards/$yard/config.env"
}

create_local_yard() {
  local yard="$1" PROJECT INSTANCE
  PROJECT=subyard; INSTANCE=yard
  if [ "$yard" != default ]; then
    PROJECT="subyard-$yard"; INSTANCE="yard-$yard"
    ! incus project show "$PROJECT" >/dev/null 2>&1 || die "refusing existing project $PROJECT"
    OWNED_EXTRA_YARDS+=("$yard")
  fi
  printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
  incus project create "$PROJECT" -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
  operator_yard -Y "$yard" init --yes
  operator_yard -Y "$yard" start --yes
}

write_settings() {
  local source="$1" destination="$2" value="$3"
  operator_env bash -c 'set -eu; jq --arg value "$3" ".env.SUBYARD_ACCEPTANCE = \$value" "$1" > "$2.next"; chmod 0600 "$2.next"; mv "$2.next" "$2"' _ "$source" "$destination" "$value"
}

assert_materialized() {
  local yard="$1" expected="$2" project=subyard instance=yard
  [ "$yard" = default ] || { project="subyard-$yard"; instance="yard-$yard"; }
  [ "$(incus list "$instance" --project "$project" --format csv -c s)" = RUNNING ] || die "expected running $yard"
  # This is the physical real-Incus boundary, reading the actual guest asset.
  [ "$(incus exec "$instance" --project "$project" -- jq -er .env.SUBYARD_ACCEPTANCE /home/dev/.claude/settings.json)" = "$expected" ] || die "materialized settings did not converge for $yard"
}

assert_excluded_yards() {
  [ "$(incus list yard-stopped --project subyard-stopped --format csv -c s)" = STOPPED ] || die 'stopped yard was activated'
  [ "$(incus config get yard-stopped user.subyard.desired_power --project subyard-stopped)" = stopped ] || die 'stopped power intent changed'
  ! incus project show subyard-absent >/dev/null 2>&1 || die 'absent yard was provisioned'
  ! incus project show subyard-remote >/dev/null 2>&1 || die 'remote yard was provisioned locally'
  local yard
  for yard in stopped absent remote; do
    sudo -n cmp "$STATE_ROOT/$yard.retained" "$CONFIG_HOME/yards/$yard/config.env" || die "unrelated $yard registration changed"
  done
}

candidate_update() {
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" "$@"
}

recovery_state_digest() {
  operator_env python3 - "$CONFIG_HOME/release-transition/recovery" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
digest = hashlib.sha256()
for path in sorted(root.rglob('*')):
    if path.is_file():
        digest.update(str(path.relative_to(root)).encode() + b'\0' + path.read_bytes())
print(digest.hexdigest())
PY
}

assert_no_consent() {
  local operation="$1" output="$STATE_ROOT/no-consent-$1.log" rc=0 before
  before="$(recovery_state_digest)"
  if [ "$operation" = migrate ]; then
    operator_yard migrate </dev/null > "$output" 2>&1 || rc=$?
  else
    candidate_update </dev/null > "$output" 2>&1 || rc=$?
  fi
  [ "$rc" = 1 ] && grep -Fq 'confirmation required: interactive terminal required' "$output" || die "$operation did not require fresh consent"
  sudo -n cmp "$ACTIVATION_JOURNAL_BASELINE" "$V2_JOURNAL" || die "$operation changed interrupted journal without consent"
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER" || die "$operation changed migration ledger without consent"
  [ "$(recovery_state_digest)" = "$before" ] || die "$operation changed recovery evidence without consent"
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
}

install_pre_cas_fault() {
  local temporary="$STATE_ROOT/pre-cas-systemctl"
  printf '%s\n' '#!/bin/sh' 'set -eu' \
    "journal=$V2_JOURNAL" "predecessor=$1" \
    "receipts=$CONFIG_HOME/release-transition/recovery/v2/transactions" \
    "probe=$ACTIVATION_FAULT_PROBE.pre-cas" \
    'if [ "${1:-}" = show ] && [ "${2:-}" = subyard-power-reconcile.service ] &&' \
    '  [ -d "$receipts" ] && [ ! -e "$probe" ] &&' \
    '  [ "$(/usr/bin/jq -er .transaction "$journal")" = "$predecessor" ]; then' \
    '  for receipt in "$receipts"/*.json; do' \
    '    [ -f "$receipt" ] || continue' \
    '    printf "%s\n" "receipt published before journal CAS" > "$probe"' \
    '    exit 75' \
    '  done' \
    'fi' \
    "exec $ACTIVATION_SYSTEMCTL_DELEGATE \"\$@\"" > "$temporary"
  sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0755 "$temporary" "$ACTIVATION_SYSTEMCTL_WRAPPER"
}

run_acceptance() {
  local yard source host_settings before_tx replacement receipt reserved cancelled rc=0
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  release_candidate_prepare "$ROOT" >/dev/null
  CANDIDATE_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  [ "$CANDIDATE_VERSION" != "$OLD_VERSION" ] || die 'candidate must differ from pinned predecessor'
  ensure_platform
  prepare_fixture
  mkdir -p "$RELEASE_ROOT"
  cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
  chmod -R a+rX "$RELEASE_ROOT"
  if ! incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1; then
    timeout --signal=TERM --kill-after=10 900 sudo -n /usr/bin/incus image copy images:debian/13/cloud local: --alias "$BASE_IMAGE" --project default </dev/null >/dev/null
    CREATED_IMAGE="$(image_fingerprint)"
  fi
  info "installing checksum-pinned published predecessor $OLD_VERSION"
  download_baseline
  operator_yard config set CODING_TOOL_INTEGRATIONS claude --scope host --local --yes
  seed_yard named 2233
  seed_yard stopped 2234
  create_local_yard default
  create_local_yard named
  create_local_yard stopped
  operator_yard -Y stopped stop --yes
  OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  source="$OPERATOR_HOME/.subyard/runtime/current/config/agents/claude/settings.json"
  write_settings "$source" "$OPERATOR_HOME/default-settings.json" default-v1
  write_settings "$source" "$OPERATOR_HOME/named-settings.json" named-v1
  operator_yard config import AGENT_claude_CONFIG "$OPERATOR_HOME/default-settings.json" --scope host --local --yes
  operator_yard -Y named config import AGENT_claude_CONFIG "$OPERATOR_HOME/named-settings.json" --scope yard --local --yes
  host_settings="$CONFIG_HOME/overrides/host/agents/claude/settings.json"
  operator_env test -f "$host_settings" || die 'public import did not create canonical host asset'
  seed_yard absent 2235
  # Unrelated explicit-empty template must survive source migration classification.
  operator_env bash -c 'printf "YARD_TEMPLATE=\047\047\n" >> "$1"' _ "$CONFIG_HOME/yards/absent/config.env"
  seed_yard remote 2236
  operator_env bash -c 'printf "ACCESS_KIND=remote\nOWNER_ENDPOINT=unreachable.invalid\nOWNER_YARD_NAME=default\n" >> "$1"' _ "$CONFIG_HOME/yards/remote/config.env"
  for yard in stopped absent remote; do
    sudo -n install -m 0600 "$CONFIG_HOME/yards/$yard/config.env" "$STATE_ROOT/$yard.retained"
  done
  info 'ordinary public update must converge default and named persistent settings'
  candidate_update --yes
  CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  assert_materialized default default-v1
  assert_materialized named named-v1
  assert_excluded_yards
  operator_env grep -Fxq 'SSH_HOST=fixture-host' "$CONFIG_HOME/yards/named/config.env"
  operator_yard config status --all-local
  sudo -n jq -e '.checkpoint == "complete"' "$V2_JOURNAL" >/dev/null
  sudo -n install -m 0600 "$V2_LEDGER" "$ACTIVATION_LEDGER_BASELINE"

  info 'interrupting actual same-release activation reconciliation through owned systemctl wrapper'
  materialize_unit "$OLD_UNIT_FIXTURE" "$STATE_ROOT/drift.service"
  sudo -n install -m 0644 "$STATE_ROOT/drift.service" "$UNIT"
  sudo -n systemctl daemon-reload
  install_activation_reconcile_fault
  candidate_update --yes > "$STATE_ROOT/interruption.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] && operator_env test -f "$ACTIVATION_FAULT_PROBE" || die 'real activation fault was not reached'
  [ "$(operator_env wc -l "$ACTIVATION_FAULT_PROBE" | awk '{print $1}')" = 1 ] || die 'activation fault fired more than once'
  operator_env find "$ACTIVATION_SYSTEMCTL_WRAPPER" -delete
  sudo -n jq '{checkpoint, steps: (.steps | length), sameRelease: (.releases.from == .releases.target)}' "$V2_JOURNAL"
  sudo -n jq -e '.checkpoint == "reconciling" and (.steps | length) == 0 and .releases.from == .releases.target' "$V2_JOURNAL" >/dev/null \
    || die 'interruption did not retain a same-release activation-only reconciling journal'
  materialize_unit "$ROOT/.subyard-acceptance/runtime/config/systemd/subyard-power-reconcile.service.in" "$STATE_ROOT/candidate.service"
  sudo -n cmp "$STATE_ROOT/candidate.service" "$UNIT" || die 'fault did not occur after actual host-power apply'
  sudo -n install -m 0600 "$V2_JOURNAL" "$ACTIVATION_JOURNAL_BASELINE"
  before_tx="$(sudo -n jq -er .transaction "$V2_JOURNAL")"
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER"
  write_settings "$host_settings" "$host_settings" default-v2
  info 'checking the fresh recovery plan after persisted inputs change'
  operator_yard migrate --check --json > "$STATE_ROOT/fresh-plan.json"
  # Current migration reports readiness; update inspection includes the full plan.
  jq -e --arg predecessor "$before_tx" '.outcome.code == "recovery-pending" and .outcome.status == "recovering" and .outcome.transaction == $predecessor and (.blockers | length) == 0' "$STATE_ROOT/fresh-plan.json" >/dev/null \
    || die 'migrate inspection did not report admissible fresh recovery'
  candidate_update --check --json > "$STATE_ROOT/update-plan.json"
  jq '{outcome: (.outcome | {status, code}), changed: .assessment.changed, hasPlan: (.plan != null)}' "$STATE_ROOT/update-plan.json"
  jq -e --arg predecessor "$before_tx" '.outcome.code == "recovery-pending" and .outcome.transaction == $predecessor and .assessment.changed == true and .plan != null and .resume == null and (.blockers | length) == 0' "$STATE_ROOT/update-plan.json" >/dev/null \
    || die 'update inspection did not offer a fresh recovery assessment'
  [ "$(jq -r .plan "$STATE_ROOT/update-plan.json")" != "$(sudo -n jq -r .authorizationPlan "$V2_JOURNAL")" ] || die 'changed config reused predecessor authorization'
  assert_no_consent migrate
  assert_no_consent update

  info 'interrupting recovery after immutable receipt publication and before journal CAS'
  install_pre_cas_fault "$before_tx"
  rc=0
  operator_yard migrate --yes > "$STATE_ROOT/pre-cas.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] && operator_env test -f "$ACTIVATION_FAULT_PROBE.pre-cas" || die 'pre-CAS interruption was not reached'
  operator_env find "$ACTIVATION_SYSTEMCTL_WRAPPER" -delete
  sudo -n cmp "$ACTIVATION_JOURNAL_BASELINE" "$V2_JOURNAL" || die 'pre-CAS interruption changed predecessor'
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER" || die 'pre-CAS interruption changed ledger'
  reserved="$(operator_env python3 -c 'import pathlib,sys; paths=list(pathlib.Path(sys.argv[1]).glob("*.json")); assert len(paths)==1; print(paths[0])' "$CONFIG_HOME/release-transition/recovery/v2/transactions")"
  cancelled="$(sudo -n jq -er .successor.transaction "$reserved")"
  sudo -n install -m 0600 "$reserved" "$STATE_ROOT/cancelled-receipt.json"
  write_settings "$host_settings" "$host_settings" default-v3
  candidate_update --check --json > "$STATE_ROOT/update-plan.json"
  jq -e '.assessment.changed == true and .resume == null and (.blockers | length) == 0' "$STATE_ROOT/update-plan.json" >/dev/null
  [ "$(jq -r .plan "$STATE_ROOT/update-plan.json")" != "$(sudo -n jq -r .successor.authorizationPlan "$reserved")" ] || die 'changed inputs reused reserved plan'
  assert_no_consent migrate
  assert_no_consent update

  info 'fresh explicit consent must publish replacement receipt and reach ready'
  operator_yard migrate --yes
  replacement="$(sudo -n jq -er .transaction "$V2_JOURNAL")"
  [ "$replacement" != "$before_tx" ] || die 'recovery overwrote predecessor transaction'
  sudo -n jq -e '.checkpoint == "complete" and (.steps | length) == 0' "$V2_JOURNAL" >/dev/null
  receipt="$CONFIG_HOME/release-transition/recovery/v2/transactions/$replacement.json"
  sudo -n jq -e --slurpfile predecessor "$ACTIVATION_JOURNAL_BASELINE" --slurpfile plan "$STATE_ROOT/update-plan.json" \
    '.schemaVersion == 2 and .contract == "activation-only-replacement-v2" and .basePlan != null and .reservation != null and .predecessor == $predecessor[0] and .replacement.transaction == $predecessor[0].transaction and .successor.authorizationPlan == $plan[0].plan and .successor.authorizationDigest != .predecessor.authorizationDigest and .successor.observationScope != .predecessor.observationScope and (.nativePlans | length) > 0' "$receipt" >/dev/null
  operator_env test ! -e "$reserved" || die 'cancelled live reservation remains selected'
  sudo -n jq -e --slurpfile original "$STATE_ROOT/cancelled-receipt.json" --slurpfile plan "$STATE_ROOT/update-plan.json" \
    '.receipt == $original[0] and .cancellation.plan == $plan[0].plan and .completed == null' \
    "$CONFIG_HOME/release-transition/recovery/v2/archive/$cancelled.json" >/dev/null
  [ "$(sudo -n jq -er .ledgerFingerprint "$receipt")" = "$(sudo -n sha256sum "$ACTIVATION_LEDGER_BASELINE" | awk '{print $1}')" ] || die 'receipt did not preserve ledger fingerprint'
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER"
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  assert_materialized default default-v3
  assert_materialized named named-v1
  assert_excluded_yards
  operator_yard migrate --check --json > "$STATE_ROOT/ready.json"
  jq -e '.outcome.code == "ready" and (.blockers | length) == 0' "$STATE_ROOT/ready.json" >/dev/null
  sudo -n install -m 0600 "$V2_JOURNAL" "$STATE_ROOT/completed-journal.json"
  sudo -n install -m 0600 "$receipt" "$STATE_ROOT/completed-receipt.json"
  candidate_update --yes
  sudo -n cmp "$STATE_ROOT/completed-journal.json" "$V2_JOURNAL" || die 'ready repeat changed successor journal'
  sudo -n cmp "$STATE_ROOT/completed-receipt.json" "$receipt" || die 'ready repeat changed recovery receipt'
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER" || die 'ready repeat changed migration ledger'
  assert_materialized default default-v3
  assert_materialized named named-v1
  assert_excluded_yards
  # A short root-only regression covers the ownership case unavailable on dev hosts.
  p0_capacity_reset_build_cache
  go test -c ./internal/config -o "$STATE_ROOT/config.test"
  sudo -n "$STATE_ROOT/config.test" -test.run '^TestPersistentSourceDiagnosticsDistinguishSafetyChecks$/ownership$' -test.v
  ok "candidate $CANDIDATE_VERSION: real default/named convergence and changed-scope recovery passed"
}

for command in sudo systemctl timeout cmp curl go jq sha256sum python3; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable worker'
run_acceptance
