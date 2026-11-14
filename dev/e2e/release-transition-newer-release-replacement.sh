#!/usr/bin/env bash
# Refused yards report a named verdict without changing anything. A newer release
# replaces unfinished journals before and after activation, replaces its own failed
# replacement, and does so through the active launcher, on an allocated disposable VM.
set -euo pipefail

PREFLIGHT=0
if [ "${1:-}" = --preflight ]; then PREFLIGHT=1; shift; fi
TOKEN="${1:-}"
# Reuse its marked Incus project, isolated runtime paths, native environment and
# bounded cleanup. The shared library has no historical acceptance entrypoint.
# shellcheck source=dev/e2e/release-transition-fixture.sh
. "$(dirname "${BASH_SOURCE[0]}")/release-transition-fixture.sh" "$TOKEN"

STATE_ROOT="/var/tmp/subyard-newer-release-$TOKEN"
# shellcheck source=dev/e2e/lib-power-upgrade-contracts.sh
. "$ROOT/dev/e2e/lib-power-upgrade-contracts.sh"
SUCCESS_ROOT="$STATE_ROOT/success"
MARKER="subyard-newer-release-$TOKEN"
SUCCESS_YARD="newer-release-$TOKEN"
IFS=$'\t' read -r SOURCE_VERSION SOURCE_INSTALLER_SHA256 SOURCE_AMD64_SHA256 _ < <(
  published_anchor
)
SOURCE_BUNDLE="subyard-$SOURCE_VERSION-linux-amd64.tar.gz"
ASSET_ROOT="$STATE_ROOT/assets"
SOURCE_ASSETS="$ASSET_ROOT/source"
VERSIONS=(0.99.1 0.99.2 0.99.3 0.99.4 0.99.5)
LAUNCHER=

die() { printf 'release-transition-newer-release-replacement: %s\n' "$*" >&2; exit 2; }

# Adoption owns user.subyard.*. Keep fixture authority outside that namespace,
# including every inherited cleanup guard that calls project_is_marked.
project_is_marked() {
  incus project show "$PROJECT" >/dev/null 2>&1 \
    && [ "$(incus project get "$PROJECT" user.subyard-e2e.newer-release 2>/dev/null)" = "$FIXTURE_MARKER" ]
}

# Public cleanup retains native coordination files. Bind only that validated,
# unchanged metadata; the historical fixture still requires absent power artifacts.
prepare_newer_fixture() {
  local path artifact
  select_fixture success
  if [ -z "$POWER_COORDINATION_BINDING" ]; then
    for path in "$POWER_RECONCILER" "$POWER_UNIT"; do
      for artifact in "${path%/*}/.${path##*/}.native-install.lock" "${path%/*}/.${path##*/}.native-custody.json"; do
        if [ -e "$artifact" ] || [ -L "$artifact" ]; then
          POWER_COORDINATION_BINDING="$(power_coordination_binding)" \
            || die 'cannot bind retained power coordination before newer-release fixture'
          break 2
        fi
      done
    done
  fi
  prepare_fixture success
}

cleanup_newer_release() {
  local rc=$? teardown_rc=0 cleanup_rc=0 launcher evidence="$ROOT/.build/newer-release-$TOKEN"
  set +e
  if [ -d "$SUCCESS_ROOT" ] && [ ! -L "$SUCCESS_ROOT" ]; then
    install -d -m 0700 "$evidence"
    find "$SUCCESS_ROOT" -maxdepth 1 -type f -exec cp -- {} "$evidence/" \;
    [ ! -d "$CONFIG_HOME/release-transition" ] || cp -a "$CONFIG_HOME/release-transition" "$evidence/protected-state"
    if [ "$CLEANUP_ARMED" = 1 ] && [ "$SUCCESS_ARMED" = 1 ] && incus project show "$PROJECT" >/dev/null 2>&1; then
      (
        assert_fixture_root && assert_project_marker || exit 3
        launcher="$(readlink -f -- "$RUNTIME_ROOT/current/bin/yard")" || exit 3
        case "$launcher" in "$RUNTIME_ROOT"/releases/*/bin/yard) ;; *) exit 3 ;; esac
        fixture_env "$launcher" -Y "$YARD_NAME" teardown --yes
      ) > "$evidence/native-teardown.log" 2>&1
      teardown_rc=$?
    fi
    ( (exit 0); cleanup ) > "$evidence/cleanup.log" 2>&1
    cleanup_rc=$?
    [ "$teardown_rc" = 0 ] && [ "$cleanup_rc" = 0 ] || rc=3
    exit "$rc"
  fi
  (exit "$rc")
  cleanup
}

fetch_source_assets() {
  local name
  [ -n "$SOURCE_VERSION" ] || die 'missing direct anchor pin'
  install -d -m 0700 "$SOURCE_ASSETS"
  for name in "$SOURCE_BUNDLE" "$SOURCE_BUNDLE.sha256" "$SOURCE_BUNDLE.manifest.json" \
    "$SOURCE_BUNDLE.provenance.json" subyard-install-runtime-release.sh subyard-install-runtime-release.sh.sha256 subyard-install.sh; do
    curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$SOURCE_ASSETS/$name" \
      "https://github.com/Subyard/Subyard/releases/download/v$SOURCE_VERSION/$name"
  done
  [ "$(sha256sum "$SOURCE_ASSETS/$SOURCE_BUNDLE" | awk '{print $1}')" = "$SOURCE_AMD64_SHA256" ] \
    && [ "$(sha256sum "$SOURCE_ASSETS/subyard-install.sh" | awk '{print $1}')" = "$SOURCE_INSTALLER_SHA256" ] \
    || die 'published source differs from its immutable pin'
}

stage() { # version: sets LAUNCHER to the published release's own launcher
  local version="$1" log="$FIXTURE_ROOT/stage-$1.log" root
  fixture_env env YARD_RELEASE_BASE_URL="file://$ASSET_ROOT/$version" \
    bash "$ASSET_ROOT/$version/subyard-install.sh" --publish-only --version "$version" > "$log"
  root="$(sed -n 's/^staged release=.* candidate=//p' "$log")"
  [ -d "$root" ] || die "release $version was not staged"
  LAUNCHER="$root/bin/yard"
}

active_version() {
  fixture_env "$RUNTIME_ROOT/current/bin/yard" --version
}

interrupt_at_reconciling() { # version
  local version="$1" marker="$FIXTURE_ROOT/interrupted-$1" checkpoint="${2:-reconciling}"
  fixture_env python3 - "$ROOT" "$LAUNCHER" "$version" "$marker" "$JOURNAL" "$RUNTIME_ROOT" "$checkpoint" <<'PY'
import json, os, pathlib, runpy, sys
root, launcher, version, marker, journal, runtime, checkpoint = sys.argv[1:]
module = runpy.run_path(str(pathlib.Path(root) / "dev/verify-release-upgrades.py"))
# A replaced journal may already be at the checkpoint; observe only its successor.
predecessor = json.loads(pathlib.Path(journal).read_text())["transaction"] if pathlib.Path(journal).is_file() else None
result = module["run_process"](
    [launcher, "update", "--offline", "--version", version, "--yes", "--runtime-root", runtime],
    os.environ.copy(), timeout=600,
    interrupt_checkpoint=(pathlib.Path(journal), checkpoint, pathlib.Path(marker), predecessor),
    allow_failed_checkpoint=True)
pathlib.Path(marker + ".stdout").write_text(result.stdout)
pathlib.Path(marker + ".stderr").write_text(result.stderr)
if result.returncode == 0 or not pathlib.Path(marker).is_file():
    raise SystemExit("transition did not reach the observed interruption: " + result.stdout + result.stderr)
PY
}

require_replaced() { # saved journal copy
  local saved="$1" transaction fingerprint
  transaction="$(jq -er .transaction "$saved")"
  fingerprint="$(sha256sum "$saved" | awk '{print $1}')"
  cmp "$saved" "$CONFIG_HOME/release-transition-replaced-journals/v1/$transaction-$fingerprint.json" \
    || die "journal $transaction was not preserved"
  [ "$(jq -er .transaction "$JOURNAL")" != "$transaction" ] \
    || die "journal $transaction was not replaced: saved=$(jq -c '{transaction,checkpoint,goal,releases}' "$saved") current=$(jq -c '{transaction,checkpoint,goal,releases}' "$JOURNAL") interrupted=$(cat "$FIXTURE_ROOT"/interrupted-* 2>/dev/null | tr '\n' ' ')"
}

require_fresh_plan() { # version
  fixture_env "$LAUNCHER" update --check --offline --version "$1" --json > "$FIXTURE_ROOT/plan-$1.json"
  jq -e '.resume==null and .assessment.changed and (.blockers|length)==0 and
    any(.assessment.consequences[];startswith("replace the unfinished release transition"))' \
    "$FIXTURE_ROOT/plan-$1.json" >/dev/null || die "release $1 did not offer a fresh replacement plan"
}

journal_state() {
  if [ -e "$JOURNAL" ]; then sha256sum "$JOURNAL"; else printf 'absent\n'; fi
}

# A refused yard reports its name and reason as a verdict, and nothing changes.
require_refusal() { # case reason...
  local name="$1" log="$FIXTURE_ROOT/refusal-$1" before active reason rc=0
  shift
  before="$(journal_state)"
  active="$(readlink "$RUNTIME_ROOT/current")"
  fixture_env "$LAUNCHER" update --check --offline --version "${VERSIONS[0]}" --json \
    > "$log.json" 2> "$log.stderr" || rc=$?
  printf 'refusal %s: exit=%s\n' "$name" "$rc"
  jq -c '.blockers' "$log.json" || true
  cat "$log.stderr"
  # A failed native read can also block later inspections, so only migrations must be verdicts.
  jq -e --arg yard "yard $YARD_NAME" 'any(.blockers[];.resource|startswith("migration.")) and
    all(.blockers[];select(.resource|startswith("migration."))|.code=="precondition-blocked") and
    all(.blockers[];(.message|contains($yard)) and (.retry|contains("install a compatible release")|not))' \
    "$log.json" >/dev/null || die "$name: refusal is not a named verdict"
  for reason in "$@"; do
    jq -e --arg reason "$reason" 'any(.blockers[];.message|contains($reason))' "$log.json" >/dev/null \
      || die "$name: verdict omits: $reason"
    grep -Fq "release transition refusal: yard $YARD_NAME: " "$log.stderr" && grep -Fq "$reason" "$log.stderr" \
      || die "$name: diagnostic stream omits: $reason"
  done
  [ "$(journal_state)" = "$before" ] || die "$name: refused check changed the journal"
  [ "$(readlink "$RUNTIME_ROOT/current")" = "$active" ] || die "$name: refused check changed the active release"
}

check_refusals() {
  local srv foreign=user.subyard.disk_tmp srv_reason='a missing or different srv device'
  local foreign_reason='foreign guest temporary policy metadata' plan="$FIXTURE_ROOT/refusal-plan.json"
  # These are named migration-precondition verdicts. A candidate at the anchor's
  # epochs updates without migration steps, so there is nothing to refuse.
  fixture_env "$LAUNCHER" update --check --offline --version "${VERSIONS[0]}" --json > "$plan" 2>/dev/null || true
  if ! jq -e '[.. | objects | .resource? | strings | select(startswith("migration."))] | length > 0' "$plan" >/dev/null; then
    printf 'refusals: no candidate migration from %s; skipped named migration verdicts\n' "$SOURCE_VERSION"
    return 0
  fi
  srv="$(incus query "/1.0/instances/$INSTANCE?project=$PROJECT" | jq -c .devices.srv)"
  [ "$srv" != null ] || die 'srv is not a local device'
  [ "$(incus config get "$INSTANCE" "$foreign" --project "$PROJECT")" = v1 ] || die 'source temporary policy is not current'
  local settings=()
  mapfile -t settings < <(jq -r 'del(.type)|to_entries[]|"\(.key)=\(.value)"' <<<"$srv")

  incus config set "$INSTANCE" "$foreign" foreign --project "$PROJECT"
  require_refusal metadata "$foreign_reason"
  incus config set "$INSTANCE" "$foreign" v1 --project "$PROJECT"

  incus config device remove "$INSTANCE" srv --project "$PROJECT" >/dev/null
  require_refusal srv "$srv_reason"
  incus config device add "$INSTANCE" srv disk "${settings[@]}" --project "$PROJECT" >/dev/null
  [ "$(incus query "/1.0/instances/$INSTANCE?project=$PROJECT" | jq -c .devices.srv)" = "$srv" ] \
    || die 'srv device was not restored'
}

require_ready() { # version
  fixture_env "$LAUNCHER" update --check --offline --version "$1" --json > "$FIXTURE_ROOT/ready-$1.json"
  jq -e '.outcome.status=="ready" and .outcome.reachedGoal' "$FIXTURE_ROOT/ready-$1.json" >/dev/null \
    || die "release $1 did not reach ready"
  [ "$(active_version)" = "yard $1" ] || die "release $1 is not active"
  fixture_env "$LAUNCHER" -Y "$YARD_NAME" check
  incus exec "$INSTANCE" --project "$PROJECT" -- grep -Fxq "$FIXTURE_MARKER" /var/tmp/subyard-newer-release-retained \
    || die 'guest data did not survive'
}

main_newer_release() {
  valid_token "$TOKEN" || die 'numeric allocation token required'
  if [ "$PREFLIGHT" = 1 ]; then
    bash -n "${BASH_SOURCE[0]}"
    shellcheck -x -S warning "${BASH_SOURCE[0]}"
    printf 'ok: newer release replacement script preflight passed\n'
    return
  fi
  [ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run only on allocated disposable VM1'
  command -v curl >/dev/null && command -v incus >/dev/null && command -v jq >/dev/null && command -v python3 >/dev/null && command -v go >/dev/null
  incus info >/dev/null
  [ ! -e "$STATE_ROOT" ] || die 'fixture state already exists'
  trap cleanup_newer_release EXIT
  trap 'handle_signal 130' INT
  trap 'handle_signal 143' TERM
  install -d -m 0700 "$STATE_ROOT" "$ASSET_ROOT"
  printf '%s\n' "$MARKER" > "$STATE_ROOT/.subyard-p0-marker"
  CLEANUP_ARMED=1
  fetch_source_assets
  local version
  for version in "${VERSIONS[@]}"; do
    bash "$ROOT/dev/package-engine.sh" --version "$version" --output-dir "$ASSET_ROOT/$version" >/dev/null
  done
  prepare_newer_fixture
  local installed="$SOURCE_ASSETS/$SOURCE_BUNDLE"
  fixture_env bash "$SOURCE_ASSETS/subyard-install-runtime-release.sh" --runtime-root "$RUNTIME_ROOT" \
    --bundle "$installed" --checksum "$installed.sha256" \
    --manifest "$installed.manifest.json" --provenance "$installed.provenance.json"
  # As the installer's first install does, complete the anchor's protected
  # transition before init can use its persisted-state capabilities.
  fixture_env "$RUNTIME_ROOT/current/bin/yard" migrate --yes
  local source_launcher="$RUNTIME_ROOT/current/bin/yard"
  [ "$(fixture_env "$source_launcher" --version)" = "yard $SOURCE_VERSION" ] || die 'source release unavailable'
  ! incus project show "$PROJECT" >/dev/null 2>&1 || die 'fixture project already exists'
  # Ordinary init creates its own proven project ownership before fixture marking.
  local init_rc=0
  fixture_env "$source_launcher" -Y "$YARD_NAME" init --yes || init_rc=$?
  if [ "$init_rc" != 0 ]; then
    [ "$(incus project get "$PROJECT" user.subyard.installation)" = 1 ] \
      && [ "$(incus project get "$PROJECT" user.subyard.operator_uid)" = "$(id -u)" ] \
      && [ "$(incus project get "$PROJECT" user.subyard.data_home)" = "$DATA_ROOT" ] \
      && [ "$(incus project get "$PROJECT" user.subyard.config_home)" = "$CONFIG_HOME" ] \
      && [ -z "$(incus list --project "$PROJECT" --format csv -c n)" ] \
      || die 'source init failed outside its owned empty image namespace'
    incus project set "$PROJECT" features.images=false
  fi
  incus project set "$PROJECT" "user.subyard-e2e.newer-release=$FIXTURE_MARKER"
  [ "$init_rc" = 0 ] || fixture_env "$source_launcher" -Y "$YARD_NAME" init --yes
  fixture_env "$source_launcher" -Y "$YARD_NAME" start --yes
  fixture_env "$source_launcher" migrate --yes
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -c \
    'printf "%s\n" "$1" > /var/tmp/subyard-newer-release-retained' _ "$FIXTURE_MARKER"
  local rc source_active
  source_active="$(readlink "$RUNTIME_ROOT/current")"

  # Before activation: the anchor and candidates share epochs, so the update has no
  # migration steps. Stop it at authorization, leaving its journal unfinished.
  stage "${VERSIONS[0]}"
  check_refusals
  interrupt_at_reconciling "${VERSIONS[0]}" authorized
  jq -e '.checkpoint=="authorized"' "$JOURNAL" >/dev/null || die 'update did not stay unfinished'
  [ "$(readlink "$RUNTIME_ROOT/current")" = "$source_active" ] || die 'unfinished update changed the active release'
  cp "$JOURNAL" "$FIXTURE_ROOT/pre-journal.json"
  # An update stopped at authorization may not have written transaction evidence yet.
  local pre_evidence=0
  [ ! -d "$CONFIG_HOME/release-transition/v2/transactions/$(jq -er .transaction "$JOURNAL")" ] || pre_evidence=1
  rc=0
  fixture_env "$LAUNCHER" update --check --version "$SOURCE_VERSION" > "$FIXTURE_ROOT/older.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] && grep -Fq 'permits only exact target' "$FIXTURE_ROOT/older.log" \
    || die 'an older release was allowed to replace the journal'
  cmp "$JOURNAL" "$FIXTURE_ROOT/pre-journal.json"

  stage "${VERSIONS[1]}"
  require_fresh_plan "${VERSIONS[1]}"
  rc=0
  fixture_env "$LAUNCHER" update --offline --version "${VERSIONS[1]}" </dev/null \
    > "$FIXTURE_ROOT/decline.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] && grep -Eiq 'confirm' "$FIXTURE_ROOT/decline.log" || die 'replacement did not ask for fresh consent'
  cmp "$JOURNAL" "$FIXTURE_ROOT/pre-journal.json"
  # The replacement activates, and only reconciliation stops.
  interrupt_at_reconciling "${VERSIONS[1]}"
  require_replaced "$FIXTURE_ROOT/pre-journal.json"

  # After activation: the replacement release is already active and its transition stopped.
  jq -e '.checkpoint=="reconciling"' "$JOURNAL" >/dev/null || die 'activated journal shape differs'
  [ "$(active_version)" = "yard ${VERSIONS[1]}" ] || die 'interrupted release is not active'
  cp "$JOURNAL" "$FIXTURE_ROOT/post-journal.json"
  stage "${VERSIONS[2]}"
  require_fresh_plan "${VERSIONS[2]}"
  interrupt_at_reconciling "${VERSIONS[2]}"
  require_replaced "$FIXTURE_ROOT/post-journal.json"
  [ "$(active_version)" = "yard ${VERSIONS[2]}" ] || die 'interrupted replacement is not active'
  cp "$JOURNAL" "$FIXTURE_ROOT/first-fix-journal.json"

  # The first fix stops the same way; a later fix must still be able to take over.
  stage "${VERSIONS[3]}"
  require_fresh_plan "${VERSIONS[3]}"
  interrupt_at_reconciling "${VERSIONS[3]}"
  require_replaced "$FIXTURE_ROOT/first-fix-journal.json"
  [ "$(active_version)" = "yard ${VERSIONS[3]}" ] || die 'interrupted replacement is not active'
  cp "$JOURNAL" "$FIXTURE_ROOT/chain-journal.json"

  # The active launcher now carries this contract and fetches the fix itself.
  LAUNCHER="$RUNTIME_ROOT/current/bin/yard"
  fixture_env env YARD_RELEASE_BASE_URL="file://$ASSET_ROOT/${VERSIONS[4]}" \
    "$LAUNCHER" update --check --version "${VERSIONS[4]}" --json > "$FIXTURE_ROOT/plan-${VERSIONS[4]}.json"
  jq -e '.resume==null and (.blockers|length)==0 and
    any(.assessment.consequences[];startswith("replace the unfinished release transition"))' \
    "$FIXTURE_ROOT/plan-${VERSIONS[4]}.json" >/dev/null || die 'the active launcher did not offer a replacement'
  fixture_env env YARD_RELEASE_BASE_URL="file://$ASSET_ROOT/${VERSIONS[4]}" \
    "$LAUNCHER" update --version "${VERSIONS[4]}" --yes
  require_replaced "$FIXTURE_ROOT/chain-journal.json"
  local replaced
  # Only the closing update sweeps. Unclosed evidence moves out of the graph, while the
  # activated journal with complete evidence stays under ordinary retention.
  local pre_transaction
  pre_transaction="$(jq -er .transaction "$FIXTURE_ROOT/pre-journal.json")"
  [ ! -e "$CONFIG_HOME/release-transition/v2/transactions/$pre_transaction" ] \
    && { [ "$pre_evidence" = 0 ] || [ -d "$CONFIG_HOME/release-transition-replaced-journals/v1/$pre_transaction.transaction" ]; } \
    || die "replaced transaction with unclosed evidence stayed in the graph (evidence=$pre_evidence)"
  for replaced in first-fix chain; do
    [ ! -e "$CONFIG_HOME/release-transition/v2/transactions/$(jq -er .transaction "$FIXTURE_ROOT/$replaced-journal.json")" ] \
      || die "$replaced transaction stayed in the graph"
  done
  jq -e '.checkpoint=="complete"' "$JOURNAL" >/dev/null || die 'final replacement did not close'
  require_ready "${VERSIONS[4]}"
  printf 'ok: newer releases replaced unfinished journals before and after activation, a failed replacement, and through the active launcher\n'
}

main_newer_release "$@"
