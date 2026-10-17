#!/usr/bin/env bash
# Exact migration-ledger owner supersession on an already allocated disposable VM.
set -euo pipefail

PREFLIGHT=0
if [ "${1:-}" = --preflight ]; then PREFLIGHT=1; shift; fi
TOKEN="${1:-}"
CANDIDATE_ASSETS="${2:-}"
CANDIDATE_INPUT_VERSION="${3:-}"
SOURCE_ASSETS="${4:-}"
SOURCE_INPUT_VERSION="${5:-}"
OWNER_ASSETS="${6:-}"
HISTORY_ASSETS="${7:-}"
# Reuse its marked Incus project, isolated runtime paths, native environment and
# bounded cleanup. Sourcing never runs the frozen v0.11.1 acceptance entrypoint.
# shellcheck source=dev/e2e/release-transition-v0111-recovery.sh
. "$(dirname "${BASH_SOURCE[0]}")/release-transition-v0111-recovery.sh" "$TOKEN"

STATE_ROOT="/var/tmp/subyard-migration-owner-$TOKEN"
SUCCESS_ROOT="$STATE_ROOT/success"
POST_CAS_ROOT="$STATE_ROOT/unused"
MARKER="subyard-migration-owner-$TOKEN"
SUCCESS_YARD="migration-owner-$TOKEN"
CANDIDATE_VERSION="$CANDIDATE_INPUT_VERSION"
SOURCE_VERSION="$SOURCE_INPUT_VERSION"
OWNER_VERSION=0.21.0
HISTORY_VERSION=0.19.0

die() { printf 'release-transition-migration-owner-recovery: %s\n' "$*" >&2; exit 2; }

install_bundle() { # directory version
  local directory="$1" version="$2" bundle
  bundle="$directory/subyard-$version-linux-amd64.tar.gz"
  fixture_env bash "$CANDIDATE_ASSETS/subyard-install-runtime-release.sh" \
    --runtime-root "$RUNTIME_ROOT" --bundle "$bundle" --checksum "$bundle.sha256" \
    --manifest "$bundle.manifest.json" --provenance "$bundle.provenance.json" "${@:3}"
}

interrupt_native() { # checkpoint|successor launcher version marker
  local phase="$1" launcher="$2" version="$3" marker="$4"
  fixture_env python3 - "$ROOT" "$phase" "$launcher" "$version" "$marker" \
    "$JOURNAL" "$RUNTIME_ROOT" <<'PY'
import json, os, pathlib, runpy, signal, sys
root, phase, launcher, version, marker, journal, runtime = sys.argv[1:]
module = runpy.run_path(str(pathlib.Path(root) / "dev/verify-release-upgrades.py"))
original = json.loads(pathlib.Path(journal).read_text())["transaction"]
arguments = [launcher, "update", "--offline", "--version", version, "--yes", "--runtime-root", runtime]
options = ({"interrupt_checkpoint": (pathlib.Path(journal), "reconciling", pathlib.Path(marker)), "allow_failed_checkpoint": True}
           if phase == "checkpoint" else
           {"interrupt_replacement": (pathlib.Path(journal), original, pathlib.Path(marker))})
result = module["run_process"](arguments, os.environ.copy(), timeout=300, **options)
pathlib.Path(marker + ".stdout").write_text(result.stdout)
pathlib.Path(marker + ".stderr").write_text(result.stderr)
if result.returncode == 0 or not pathlib.Path(marker).is_file() or (phase == "successor" and result.returncode != -signal.SIGKILL):
    raise SystemExit("owned transition did not reach the observed interruption: " + result.stdout + result.stderr)
PY
}

cleanup_migration_owner() {
  local rc=$? native_rc=0 cleanup_rc=0 capture_rc=0 launcher
  local evidence="$ROOT/.build/migration-owner-$TOKEN"
  set +e
  if [ -d "$SUCCESS_ROOT" ] && [ ! -L "$SUCCESS_ROOT" ]; then
    install -d -m 0700 "$evidence" || capture_rc=1
    find "$SUCCESS_ROOT" -maxdepth 1 -type f -exec cp -- {} "$evidence/" \; || capture_rc=1
    if [ -d "$CONFIG_HOME/release-transition" ]; then
      cp -a "$CONFIG_HOME/release-transition" "$evidence/protected-state" || capture_rc=1
    fi
    # Publish-only installation does not create BIN_ROOT/yard. Use the sealed
    # current launcher for ordinary teardown before the inherited fallback.
    install -m 0600 /dev/null "$evidence/native-teardown.log" || capture_rc=1
    install -m 0600 /dev/null "$evidence/cleanup.log" || capture_rc=1
    install -m 0600 /dev/null "$evidence/cleanup-summary.json" || capture_rc=1
    if [ "$CLEANUP_ARMED" = 1 ] && [ "$SUCCESS_ARMED" = 1 ] \
      && incus project show "$PROJECT" >/dev/null 2>&1; then
      (
        assert_fixture_root && assert_project_marker || exit 3
        launcher="$(readlink -f -- "$RUNTIME_ROOT/current/bin/yard")" || exit 3
        case "$launcher" in "$RUNTIME_ROOT"/releases/*/bin/yard) ;; *) exit 3 ;; esac
        [ -x "$launcher" ] || exit 3
        fixture_env "$launcher" -Y "$YARD_NAME" teardown --yes
      ) > "$evidence/native-teardown.log" 2>&1
      native_rc=$?
    fi
    # cleanup exits by design. Keep its result distinct from the scenario and
    # native teardown, and retain diagnostics outside the disposable state root.
    ( (exit 0); cleanup ) > "$evidence/cleanup.log" 2>&1
    cleanup_rc=$?
    printf '{"scenarioExit":%d,"nativeTeardownExit":%d,"fallbackCleanupExit":%d,"captureExit":%d}\n' \
      "$rc" "$native_rc" "$cleanup_rc" "$capture_rc" > "$evidence/cleanup-summary.json"
    chmod 0600 "$evidence/native-teardown.log" "$evidence/cleanup.log" "$evidence/cleanup-summary.json" || capture_rc=1
    [ "$native_rc" = 0 ] && [ "$cleanup_rc" = 0 ] && [ "$capture_rc" = 0 ] || rc=3
    exit "$rc"
  fi
  (exit "$rc")
  cleanup
}

main_migration_owner() {
  valid_token "$TOKEN" || die 'numeric allocation token required'
   for directory in "$CANDIDATE_ASSETS" "$SOURCE_ASSETS" "$OWNER_ASSETS" "$HISTORY_ASSETS"; do
    [[ "$directory" = /* ]] && [ -d "$directory" ] && [ ! -L "$directory" ] \
      || die 'absolute asset directories required'
  done
  [[ "$CANDIDATE_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+[-A-Za-z0-9.+]*$ ]] \
    || die 'exact candidate semver required'
  [[ "$SOURCE_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+[-A-Za-z0-9.+]*$ ]] \
    || die 'exact authentic source semver required'
  [ "$(sha256sum "$OWNER_ASSETS/subyard-0.21.0-linux-amd64.tar.gz" | awk '{print $1}')" = \
    1fee6251856ede2c1cbd0c01f48d226fbce64915f712df97085d35abb4991282 ] \
    || die 'affected official owner bundle differs from the pinned published artifact'
  local directory version bundle
  for version in "$CANDIDATE_VERSION" "$SOURCE_VERSION" "$OWNER_VERSION" "$HISTORY_VERSION"; do
    case "$version" in
      "$CANDIDATE_VERSION") directory="$CANDIDATE_ASSETS" ;;
      "$SOURCE_VERSION") directory="$SOURCE_ASSETS" ;;
      "$HISTORY_VERSION") directory="$HISTORY_ASSETS" ;;
      *) directory="$OWNER_ASSETS" ;;
    esac
    bundle="subyard-$version-linux-amd64.tar.gz"
    (cd "$directory" && sha256sum --check "$bundle.sha256")
    [ -s "$directory/$bundle.manifest.json" ] && [ -s "$directory/$bundle.provenance.json" ] \
      || die 'missing authenticated bundle metadata'
  done
  if [ "$PREFLIGHT" = 1 ]; then
    bash -n "${BASH_SOURCE[0]}"
    shellcheck -x -S warning "${BASH_SOURCE[0]}"
    printf 'ok: focused migration owner recovery assets and script preflight passed\n'
    return
  fi
  [ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run only on allocated disposable VM1'
  command -v incus >/dev/null && command -v jq >/dev/null && command -v python3 >/dev/null
  incus info >/dev/null
  incus image info subyard-e2e-debian-13-cloud-container --project default >/dev/null
  [ ! -e "$STATE_ROOT" ] || die 'fixture state already exists'
  trap cleanup_migration_owner EXIT
  trap 'handle_signal 130' INT
  trap 'handle_signal 143' TERM
  install -d -m 0700 "$STATE_ROOT"
  printf '%s\n' "$MARKER" > "$STATE_ROOT/.subyard-p0-marker"
  CLEANUP_ARMED=1
  prepare_fixture success
  install_bundle "$HISTORY_ASSETS" "$HISTORY_VERSION"
  local source_launcher="$RUNTIME_ROOT/current/bin/yard" source_root history_root source_release
  history_root="$(readlink -f "$RUNTIME_ROOT/current")"
  [ "$(fixture_env "$source_launcher" --version)" = "yard $HISTORY_VERSION" ] || die 'authentic official source unavailable'
  # Exact managed project marker fences all teardown, even after interrupted updates.
  ! incus project show "$PROJECT" >/dev/null 2>&1 || die 'fixture project already exists'
  incus project create "$PROJECT" -c features.images=false -c "user.subyard.p0=$FIXTURE_MARKER" >/dev/null
  fixture_env "$source_launcher" -Y "$YARD_NAME" init --yes
  fixture_env "$source_launcher" -Y "$YARD_NAME" start --yes
  fixture_env "$source_launcher" migrate --yes
  jq -e '.checkpoint=="complete"' "$JOURNAL" >/dev/null || die 'authentic source did not close'
  cp "$LEDGER" "$FIXTURE_ROOT/projection.before"
  cp "$JOURNAL" "$FIXTURE_ROOT/official-source-journal.json"
  cp "$history_root/runtime-files.sha256" "$FIXTURE_ROOT/history-manifest.before"
  source_release="$(install_bundle "$SOURCE_ASSETS" "$SOURCE_VERSION" --publish-only)"
  source_root="$RUNTIME_ROOT/$source_release"
  source_launcher="$source_root/bin/yard"
  [ "$(fixture_env "$source_launcher" --version)" = "yard $SOURCE_VERSION" ] || die 'authentic custom source unavailable'
  # Publication leaves the updater cache empty; fetch the exact local assets natively.
  fixture_env env YARD_RELEASE_BASE_URL="file://$SOURCE_ASSETS" \
    "$source_launcher" update --version "$SOURCE_VERSION" --yes
  jq -e '.checkpoint=="complete" and .steps==[]' "$JOURNAL" >/dev/null \
    || die 'same-registry custom update did not close with zero steps'
  cp "$JOURNAL" "$FIXTURE_ROOT/custom-source-journal.json"
  cmp "$LEDGER" "$FIXTURE_ROOT/projection.before" || die 'same-registry source update changed projection'
  fixture_env python3 - "$CONFIG_HOME/release-transition/v2/transactions" > "$FIXTURE_ROOT/retained-source-transactions.json" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
print(json.dumps(sorted(p.name for p in root.iterdir()) if root.exists() else []))
PY
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -c \
    'printf "%s\n" "$1" > /var/tmp/subyard-migration-retained' _ "$FIXTURE_MARKER"

  # Publish verified 0.21 assets without selecting them. Its own real transition
  # performs both ledger-only steps before this observed reconciling interruption.
  fixture_env env YARD_RELEASE_BASE_URL="file://$OWNER_ASSETS" \
    bash "$CANDIDATE_ASSETS/subyard-install.sh" --publish-only --version "$OWNER_VERSION" \
    > "$FIXTURE_ROOT/owner-stage.log"
  local owner_root owner_launcher predecessor candidate_root candidate_launcher transaction rc
  owner_root="$(sed -n 's/^staged release=.* candidate=//p' "$FIXTURE_ROOT/owner-stage.log")"
  [ -d "$owner_root" ] || die 'official owner was not staged'
  owner_launcher="$owner_root/bin/yard"
  cp "$owner_root/runtime-files.sha256" "$FIXTURE_ROOT/owner-manifest.before"
  cp "$source_root/runtime-files.sha256" "$FIXTURE_ROOT/source-manifest.before"
  interrupt_native checkpoint "$owner_launcher" "$OWNER_VERSION" "$FIXTURE_ROOT/owner-interrupted"
  cp "$JOURNAL" "$FIXTURE_ROOT/predecessor.json"
  predecessor="$(jq -er '.transaction' "$JOURNAL")"
  jq -e '.checkpoint=="reconciling" and [.steps[].migration]==
    ["introduce-owner-publication-state-v1","localize-ssh-port-settings-v3"] and
    all(.steps[];.checkpoint=="verified")' "$JOURNAL" >/dev/null || die 'owner journal shape differs'
  jq -e --arg source "${source_root##*/}" --arg previous "${history_root##*/}" \
    '.releases.from==$source and .releases.previous==$previous' "$JOURNAL" >/dev/null \
    || die 'failed owner lost its exact authentic source generations'
  [ -d "$source_root" ] && [ -d "$history_root" ] || die 'authentic sealed source generation missing'
  cmp "$LEDGER" "$FIXTURE_ROOT/projection.before" || die 'frozen projection changed'
  rc=0
  fixture_env "$owner_launcher" update --check --offline --version "$OWNER_VERSION" \
    > "$FIXTURE_ROOT/old-reader.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] || grep -Fq operator-action-required "$FIXTURE_ROOT/old-reader.log" \
    || die 'actual defective owner inspection unexpectedly passed'
  grep -Fq 'journaled ledger before state is not a registry prefix' "$FIXTURE_ROOT/old-reader.log" \
    || die 'actual old owner did not reproduce the independent-domain reader defect'

  fixture_env env YARD_RELEASE_BASE_URL="file://$CANDIDATE_ASSETS" \
    bash "$CANDIDATE_ASSETS/subyard-install.sh" --publish-only --version "$CANDIDATE_VERSION" \
    > "$FIXTURE_ROOT/candidate-stage.log"
  candidate_root="$(sed -n 's/^staged release=.* candidate=//p' "$FIXTURE_ROOT/candidate-stage.log")"
  [ -d "$candidate_root" ] || die 'candidate was not staged'
  candidate_launcher="$candidate_root/bin/yard"
  fixture_env "$candidate_launcher" update --offline --version "$CANDIDATE_VERSION" --check --json \
    > "$FIXTURE_ROOT/plan.json"
  jq -e '.resume==null and .assessment.changed and (.blockers|length)==0' \
    "$FIXTURE_ROOT/plan.json" >/dev/null || die 'fresh candidate proof is blocked'
  cp "$JOURNAL" "$FIXTURE_ROOT/before-decline.json"
  rc=0
  fixture_env "$candidate_launcher" update --offline --version "$CANDIDATE_VERSION" \
    </dev/null > "$FIXTURE_ROOT/decline.log" 2>&1 || rc=$?
  [ "$rc" != 0 ] && grep -Eiq 'confirmation|confirm' "$FIXTURE_ROOT/decline.log" \
    || die 'candidate reused old consent'
  cmp "$JOURNAL" "$FIXTURE_ROOT/before-decline.json"
  interrupt_native successor "$candidate_launcher" "$CANDIDATE_VERSION" "$FIXTURE_ROOT/candidate-interrupted"
  transaction="$(jq -er '.transaction|select(startswith("migration-owner-v1-"))' "$JOURNAL")"
  local receipt="$CONFIG_HOME/release-transition/migration-owner/v1/transactions/$transaction.json"
  cp "$receipt" "$FIXTURE_ROOT/receipt.before"
  fixture_env "$candidate_launcher" update --offline --version "$CANDIDATE_VERSION" </dev/null
  jq -e --arg transaction "$transaction" '.checkpoint=="complete" and .transaction==$transaction' "$JOURNAL" >/dev/null
  fixture_env "$candidate_launcher" update --check --offline --version "$CANDIDATE_VERSION" --json \
    > "$FIXTURE_ROOT/ready.json"
  jq -e '.outcome.status=="ready" and .outcome.reachedGoal' "$FIXTURE_ROOT/ready.json" >/dev/null
  cmp "$receipt" "$FIXTURE_ROOT/receipt.before"
  cmp "$LEDGER" "$FIXTURE_ROOT/projection.before"
  jq -e --slurpfile before "$FIXTURE_ROOT/predecessor.json" \
    '.predecessor==$before[0] and .predecessor.checkpoint=="reconciling"' "$receipt" >/dev/null
  [ -d "$CONFIG_HOME/release-transition/v2/transactions/$predecessor" ]
  [ -d "$owner_root" ] && [ -d "$candidate_root" ]
  fixture_env "$candidate_launcher" -Y "$YARD_NAME" check
  incus exec "$INSTANCE" --project "$PROJECT" -- grep -Fxq "$FIXTURE_MARKER" \
    /var/tmp/subyard-migration-retained
  cp "$JOURNAL" "$FIXTURE_ROOT/completed.before-rollback.json"
  local active_before previous_before
  active_before="$(readlink "$RUNTIME_ROOT/current")"
  previous_before="$(readlink "$RUNTIME_ROOT/previous")"
  # The ordinary rollback command assesses compatibility before requesting consent.
  # With no consent, both a typed refusal and an eligible backward plan preserve state.
  local rollback_rc=0 rollback_decision
  fixture_env env -u YARD_RELEASE_VERSION ASSUME_YES=0 \
    "$candidate_launcher" update --rollback </dev/null \
    > "$FIXTURE_ROOT/rollback-assessment.log" 2>&1 || rollback_rc=$?
  [ "$rollback_rc" -ne 0 ] || die 'rollback executed without fresh consent'
  if grep -Fq 'code=rollback-incompatible' "$FIXTURE_ROOT/rollback-assessment.log"; then
    rollback_decision=rollback-incompatible
  elif [ "$rollback_rc" = 1 ] \
    && grep -Fxq 'yard: plan update: confirmation required: interactive terminal required' "$FIXTURE_ROOT/rollback-assessment.log" \
    && grep -Eq '^Operation plan: op-[0-9a-f]+$' "$FIXTURE_ROOT/rollback-assessment.log" \
    && grep -Fq "    desired: verified active release ${owner_root##*/}" "$FIXTURE_ROOT/rollback-assessment.log"; then
    rollback_decision=confirmation-required
  else
    die 'ordinary rollback gate returned an unsupported result'
  fi
  printf '{"exitCode":%s,"decision":"%s"}\n' "$rollback_rc" "$rollback_decision" \
    > "$FIXTURE_ROOT/rollback-assessment.json"
  cmp "$JOURNAL" "$FIXTURE_ROOT/completed.before-rollback.json"
  [ "$(readlink "$RUNTIME_ROOT/current")" = "$active_before" ]
  [ "$(readlink "$RUNTIME_ROOT/previous")" = "$previous_before" ]
  cmp "$receipt" "$FIXTURE_ROOT/receipt.before"
  cmp "$LEDGER" "$FIXTURE_ROOT/projection.before"
  cmp "$owner_root/runtime-files.sha256" "$FIXTURE_ROOT/owner-manifest.before"
  cmp "$source_root/runtime-files.sha256" "$FIXTURE_ROOT/source-manifest.before"
  cmp "$history_root/runtime-files.sha256" "$FIXTURE_ROOT/history-manifest.before"
  printf 'ok: authentic source and 0.21 owner -> fresh candidate supersession, real activation, interrupted resume and ordinary rollback assessment passed\n'

}

main_migration_owner "$@"
