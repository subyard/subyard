#!/usr/bin/env bash
# A newer release replaces unfinished journals before and after activation, and
# replaces its own failed replacement, on an already allocated disposable VM.
set -euo pipefail

PREFLIGHT=0
if [ "${1:-}" = --preflight ]; then PREFLIGHT=1; shift; fi
TOKEN="${1:-}"
# Reuse its marked Incus project, isolated runtime paths, native environment and
# bounded cleanup. Sourcing never runs the frozen v0.11.1 acceptance entrypoint.
# shellcheck source=dev/e2e/release-transition-v0111-recovery.sh
. "$(dirname "${BASH_SOURCE[0]}")/release-transition-v0111-recovery.sh" "$TOKEN"

STATE_ROOT="/var/tmp/subyard-newer-release-$TOKEN"
SUCCESS_ROOT="$STATE_ROOT/success"
POST_CAS_ROOT="$STATE_ROOT/unused"
MARKER="subyard-newer-release-$TOKEN"
SUCCESS_YARD="newer-release-$TOKEN"
# From v0.25.1, init refuses the unmarked project this fixture shares its image through.
SOURCE_VERSION=0.19.0
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
  local digest name
  install -d -m 0700 "$SOURCE_ASSETS"
  while read -r digest name; do
    curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$SOURCE_ASSETS/$name" \
      "https://github.com/Subyard/Subyard/releases/download/v$SOURCE_VERSION/$name"
    printf '%s  %s\n' "$digest" "$name" | (cd "$SOURCE_ASSETS" && sha256sum --check --quiet -)
  done <<EOF
81c55205368ef891976f28ef21b303da5ef97114d80f76c6ec60e9f3a2b698a9 $SOURCE_BUNDLE
cb04eb56fd54b04a8ef49ee31603cc8fa98811ebab293240df85ddf324ad8969 $SOURCE_BUNDLE.manifest.json
d889ca7a2af94f207c344c12f64955e655bbee183b19234bb06e9ff2d053cb62 $SOURCE_BUNDLE.provenance.json
0d2cffd7622edff32ac23c321f44ad603b99d978c0b1687755485c1257d3da31 subyard-install-runtime-release.sh
EOF
  (cd "$SOURCE_ASSETS" && sha256sum "$SOURCE_BUNDLE") > "$SOURCE_ASSETS/$SOURCE_BUNDLE.sha256"
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
  local version="$1" marker="$FIXTURE_ROOT/interrupted-$1"
  fixture_env python3 - "$ROOT" "$LAUNCHER" "$version" "$marker" "$JOURNAL" "$RUNTIME_ROOT" <<'PY'
import os, pathlib, runpy, sys
root, launcher, version, marker, journal, runtime = sys.argv[1:]
module = runpy.run_path(str(pathlib.Path(root) / "dev/verify-release-upgrades.py"))
result = module["run_process"](
    [launcher, "update", "--offline", "--version", version, "--yes", "--runtime-root", runtime],
    os.environ.copy(), timeout=600,
    interrupt_checkpoint=(pathlib.Path(journal), "reconciling", pathlib.Path(marker)),
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
  [ "$(jq -er .transaction "$JOURNAL")" != "$transaction" ] || die "journal $transaction was not replaced"
}

require_fresh_plan() { # version
  fixture_env "$LAUNCHER" update --check --offline --version "$1" --json > "$FIXTURE_ROOT/plan-$1.json"
  jq -e '.resume==null and .assessment.changed and (.blockers|length)==0 and
    any(.assessment.consequences[];startswith("replace the unfinished release transition"))' \
    "$FIXTURE_ROOT/plan-$1.json" >/dev/null || die "release $1 did not offer a fresh replacement plan"
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
  prepare_fixture success
  local installed="$SOURCE_ASSETS/$SOURCE_BUNDLE"
  fixture_env bash "$SOURCE_ASSETS/subyard-install-runtime-release.sh" --runtime-root "$RUNTIME_ROOT" \
    --bundle "$installed" --checksum "$installed.sha256" \
    --manifest "$installed.manifest.json" --provenance "$installed.provenance.json"
  local source_launcher="$RUNTIME_ROOT/current/bin/yard"
  [ "$(fixture_env "$source_launcher" --version)" = "yard $SOURCE_VERSION" ] || die 'source release unavailable'
  ! incus project show "$PROJECT" >/dev/null 2>&1 || die 'fixture project already exists'
  # The shared project sees the prepared image and fences every later teardown.
  # Use a source that accepts it: from v0.25.1, init refuses an unmarked project.
  incus project create "$PROJECT" -c features.images=false -c "user.subyard-e2e.newer-release=$FIXTURE_MARKER" >/dev/null
  fixture_env "$source_launcher" -Y "$YARD_NAME" init --yes
  fixture_env "$source_launcher" -Y "$YARD_NAME" start --yes
  fixture_env "$source_launcher" migrate --yes
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -c \
    'printf "%s\n" "$1" > /var/tmp/subyard-newer-release-retained' _ "$FIXTURE_MARKER"
  local rc source_active
  source_active="$(readlink "$RUNTIME_ROOT/current")"

  # Before activation: the operator withholds the restart, so the migration stays unfinished.
  stage "${VERSIONS[0]}"
  rc=0
  fixture_env python3 -B "$ROOT/dev/e2e/migration-late-consent.py" --yard "$YARD_NAME" --answer eof -- \
    "$LAUNCHER" update --offline --version "${VERSIONS[0]}" --yes > "$FIXTURE_ROOT/pre-stuck.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] || die 'withheld restart did not leave the update unfinished'
  jq -e '.checkpoint!="complete"' "$JOURNAL" >/dev/null || die 'update did not stay unfinished'
  [ "$(readlink "$RUNTIME_ROOT/current")" = "$source_active" ] || die 'unfinished update changed the active release'
  cp "$JOURNAL" "$FIXTURE_ROOT/pre-journal.json"
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
  fixture_env python3 -B "$ROOT/dev/e2e/migration-late-consent.py" --yard "$YARD_NAME" --allow-no-prompt -- \
    "$LAUNCHER" update --offline --version "${VERSIONS[1]}" --yes
  require_replaced "$FIXTURE_ROOT/pre-journal.json"
  # This leg replaces a journal that holds evidence, so its directory must leave the graph.
  local pre_transaction
  pre_transaction="$(jq -er .transaction "$FIXTURE_ROOT/pre-journal.json")"
  [ ! -e "$CONFIG_HOME/release-transition/v2/transactions/$pre_transaction" ] \
    && [ -d "$CONFIG_HOME/release-transition-replaced-journals/v1/$pre_transaction.transaction" ] \
    || die 'replaced transaction stayed in the graph'
  require_ready "${VERSIONS[1]}"

  # After activation: the new release is already active and its transition stopped.
  stage "${VERSIONS[2]}"
  interrupt_at_reconciling "${VERSIONS[2]}"
  jq -e '.checkpoint=="reconciling"' "$JOURNAL" >/dev/null || die 'activated journal shape differs'
  [ "$(active_version)" = "yard ${VERSIONS[2]}" ] || die 'interrupted release is not active'
  cp "$JOURNAL" "$FIXTURE_ROOT/post-journal.json"

  # The first fix stops the same way; a later fix must still be able to take over.
  stage "${VERSIONS[3]}"
  require_fresh_plan "${VERSIONS[3]}"
  interrupt_at_reconciling "${VERSIONS[3]}"
  require_replaced "$FIXTURE_ROOT/post-journal.json"
  [ "$(active_version)" = "yard ${VERSIONS[3]}" ] || die 'interrupted replacement is not active'
  cp "$JOURNAL" "$FIXTURE_ROOT/chain-journal.json"

  stage "${VERSIONS[4]}"
  require_fresh_plan "${VERSIONS[4]}"
  fixture_env "$LAUNCHER" update --offline --version "${VERSIONS[4]}" --yes
  require_replaced "$FIXTURE_ROOT/chain-journal.json"
  local replaced
  for replaced in post chain; do
    [ ! -e "$CONFIG_HOME/release-transition/v2/transactions/$(jq -er .transaction "$FIXTURE_ROOT/$replaced-journal.json")" ] \
      || die "$replaced transaction stayed in the graph"
  done
  jq -e '.checkpoint=="complete"' "$JOURNAL" >/dev/null || die 'final replacement did not close'
  require_ready "${VERSIONS[4]}"
  printf 'ok: newer releases replaced unfinished journals before and after activation, and a failed replacement\n'
}

main_newer_release "$@"
