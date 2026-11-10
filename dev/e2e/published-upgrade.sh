#!/usr/bin/env bash
# One exact published release -> frozen candidate on an allocated VM: install, init, start, update.
set -Eeuo pipefail
umask 022

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_ID="${SUBYARD_E2E_RUN_ID:-}"
[[ "$RUN_ID" =~ ^[0-9a-f]{8}$ ]] \
  || { printf 'published-upgrade: run through dev/agent-e2e.sh\n' >&2; exit 2; }
TOKEN=$((16#$RUN_ID))
VERSION="${1:-}"
# shellcheck source=dev/e2e/lib-power-upgrade-contracts.sh
. "$ROOT/dev/e2e/lib-power-upgrade-contracts.sh"
IFS=$'\t' read -r ANCHOR_VERSION _ ANCHOR_SHA256 _ < <(published_anchor)
[ "$VERSION" != anchor ] || VERSION="$ANCHOR_VERSION"
PUBLISHED_MODE=direct
case "${2:-}" in refusal|chain) PUBLISHED_MODE="$2" ;; esac
published_agent_selection() {
  local selection="${1:-none}" agent
  case "$selection" in
    none) return 0 ;;
    all) printf '%s\n' 'claude codex opencode pi'; return 0 ;;
  esac
  for agent in $selection; do
    case "$agent" in
      claude|codex|opencode|pi) ;;
      *) printf 'published-upgrade: unknown agent %q\n' "$agent" >&2; return 2 ;;
    esac
  done
  printf '%s\n' "$selection"
}
[ "$#" -le 2 ] || { printf 'usage: published-upgrade.sh VERSION [AGENTS|all|none|refusal|chain]\n' >&2; exit 2; }
if [ "$PUBLISHED_MODE" = direct ]; then
  PUBLISHED_AGENTS="$(published_agent_selection "${2:-none}")" || exit 2
else
  PUBLISHED_AGENTS=''
fi

configure_published_agents() {
  [ -n "$PUBLISHED_AGENTS" ] || return 0
  # Replace the fixture's legacy alias, avoiding conflicting canonical/alias
  # values. Every pinned release supports the canonical selection setting.
  operator_env sed -i "s/^AGENTS=none$/CODING_TOOL_INTEGRATIONS='$PUBLISHED_AGENTS'/" \
    "$OPERATOR_HOME/.config/subyard/config.env"
  operator_env grep -Fxq "CODING_TOOL_INTEGRATIONS='$PUBLISHED_AGENTS'" \
    "$OPERATOR_HOME/.config/subyard/config.env" || die 'published agent selection was not configured'
}

assert_published_agent_configs() {
  local agent source destination expected observed digest_program runtime
  runtime="$OPERATOR_HOME/.subyard/runtime/$OLD_RELEASE_TARGET"
  # Compare real published init's JSON/TOML rendering with its own shipped
  # defaults; raw configs and rules retain their exact bytes. No guest contents
  # are printed, and no guest files are seeded by this fixture.
  digest_program='import hashlib,json,pathlib,sys,tomllib
p=pathlib.Path(sys.argv[1]); raw=p.read_bytes()
if p.suffix in (".json", ".toml"):
    value=tomllib.loads(raw.decode()) if p.suffix==".toml" else json.loads(raw)
    raw=json.dumps(value,sort_keys=True,separators=(",",":"),ensure_ascii=False).encode()
print(hashlib.sha256(raw).hexdigest())'
  for agent in $PUBLISHED_AGENTS; do
    case "$agent" in
      claude) source=claude/settings.json; destination=.claude/settings.json ;;
      codex) source=codex/config.toml; destination=.codex/config.toml ;;
      opencode) source=opencode/opencode.jsonc; destination=.config/opencode/opencode.jsonc ;;
      pi) source=pi/settings.json; destination=.pi/agent/settings.json ;;
    esac
    expected="$(operator_env python3 -c "$digest_program" "$runtime/config/agents/$source")" \
      || die "cannot read published $agent defaults"
    observed="$(guest python3 -c "$digest_program" "/home/dev/$destination")" \
      || die "published init did not materialize $agent"
    [ "$observed" = "$expected" ] || die "published init changed the shipped $agent defaults"
    if [ "$agent" = codex ]; then
      expected="$(operator_env sha256sum "$runtime/config/agents/codex/rules/repo.rules" | awk '{print $1}')"
      observed="$(guest sha256sum /home/dev/.codex/rules/repo.rules | awk '{print $1}')"
      [ "$observed" = "$expected" ] || die 'published init changed the shipped Codex rules'
    fi
    ok "published init materialized $agent"
  done
}
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] \
  || { printf 'published-upgrade: a frozen candidate bundle is required\n' >&2; exit 2; }
PINS="$(awk -F '\t' -v version="$VERSION" '$1 == version' "$ROOT/dev/e2e/published-upgrade-baselines.tsv")"
[ -n "$PINS" ] || { printf 'published-upgrade: unpinned baseline %q\n' "$VERSION" >&2; exit 2; }
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"

# Reuse its marked operator/project lifecycle, host-runtime snapshot and guarded cleanup.
set -- run "$TOKEN"
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"

IFS=$'\t' read -r OLD_VERSION OLD_INSTALLER_SHA256 OLD_BUNDLE_SHA256 EXPECTATION <<<"$PINS"
if [ "$PUBLISHED_MODE" = direct ]; then
  [[ "$EXPECTATION" = direct || "$EXPECTATION" = anchor ]] || die 'a refusal baseline cannot run a direct upgrade'
else
  [[ "$EXPECTATION" = refusal || ( "$PUBLISHED_MODE" = chain && "$EXPECTATION" = pin ) ]] \
    || die 'refusal and chain modes require a refusal or chain pin baseline'
fi
INSTALLER="$STATE_ROOT/subyard-install-v$OLD_VERSION.sh"
GUEST_MARKER=/var/tmp/subyard-published-upgrade-data
CACHED_IMAGE=subyard-e2e-debian-13-cloud-container

die() {
  printf 'published-upgrade: %s: %s\n' "$OLD_VERSION" "$*" >&2
  local log
  for log in "$STATE_ROOT"/*.log; do
    [ ! -f "$log" ] || tail -n 40 "$log" >&2
  done
  exit 2
}
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s: %s\n' "$OLD_VERSION" "$*"; }

guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
instance_uuid() { incus config get "$INSTANCE" volatile.uuid --project "$PROJECT"; }

assert_ready() {
  local epochs guest_marker expected="${1:-$CANDIDATE_VERSION}" target="${2:-$CANDIDATE_RELEASE_TARGET}" registry
  registry="$OPERATOR_HOME/.subyard/runtime/$target/config/release-transition.json"
  [ "$(operator_yard --version)" = "yard $expected" ] || die 'expected release is not active'
  epochs="$(operator_env jq -c .currentEpochs "$registry")"
  # shellcheck disable=SC2016 # jq binds $epochs itself.
  # The checkpoint is the authority; ledger.json stays a pinned legacy projection.
  operator_env jq -e --argjson epochs "$epochs" \
    '.schemaVersion == 1 and (.domains | with_entries(.value |= .epoch)) == $epochs' \
    "$V2_STATE_ROOT/history-checkpoint.json" >/dev/null \
    || die "migration checkpoint did not reach the candidate epochs $epochs"
  operator_env jq -e '.checkpoint == "complete"' "$V2_JOURNAL" >/dev/null \
    || die 'transition journal is not complete'
  operator_yard migrate --check --json >"$STATE_ROOT/migrate-check.log" 2>&1 \
    && jq -e '.outcome.status == "ready" and .outcome.reachedGoal == true' "$STATE_ROOT/migrate-check.log" >/dev/null \
    || die 'candidate does not report Ready after update'
  operator_yard check >"$STATE_ROOT/check.log" 2>&1 || die 'yard check failed after update'
  [ "$(instance_uuid)" = "$UUID_BEFORE" ] || die 'update replaced the yard instance'
  guest_marker="$(guest cat "$GUEST_MARKER" </dev/null)" || die 'update lost guest data'
  [ "$guest_marker" = "$MARKER" ] || die "update changed guest data: '$guest_marker'"
}

protected_settings_digest() {
  operator_env python3 - "$OPERATOR_HOME/.config/subyard" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1]); result = hashlib.sha256()
for path in sorted(root.rglob('*')):
    result.update(str(path.relative_to(root)).encode() + b'\0')
    result.update(str(path.lstat().st_mode).encode() + b'\0')
    if path.is_symlink(): result.update(str(path.readlink()).encode())
    elif path.is_file(): result.update(path.read_bytes())
print(result.hexdigest())
PY
}

assert_source_refusal() {
  local settings before_links arguments status
  settings="$(protected_settings_digest)"
  before_links="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  for arguments in update check; do
    status=0
    if [ "$arguments" = check ]; then set -- --check; else set --; fi
    operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" YARD_RELEASE_VERSION="$CANDIDATE_VERSION" \
      "$OPERATOR_HOME/.local/bin/yard" update "$@" >"$STATE_ROOT/refusal-$arguments.log" 2>"$STATE_ROOT/refusal-$arguments.stderr" || status=$?
    if [ "$status" = 0 ]; then
      [ "$arguments" = check ] && jq -e '.outcome.reachedGoal == false and
        (.outcome.status | type == "string" and . != "ready" and . != "")' "$STATE_ROOT/refusal-$arguments.log" >/dev/null \
        || die "$arguments did not report refusal (exit=$status)"
    fi
    grep -Fq "yard update --version $ANCHOR_VERSION" "$STATE_ROOT/refusal-$arguments.stderr" \
      || die "$arguments lost the anchor command (exit=$status)"
    [ "$(protected_settings_digest)" = "$settings" ] \
      && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$before_links" ] \
      && [ "$(instance_uuid)" = "$UUID_BEFORE" ] \
      && [ "$(incus list --project "$PROJECT" --format csv -c s)" = RUNNING ] \
      && [ "$(guest cat "$GUEST_MARKER")" = "$MARKER" ] \
      || die 'refusal changed settings, runtime, instance, power or data'
  done
}

# Cleanup removes the journal, so a failed update reports its stopping point first.
report_failed_transition() {
  operator_env jq -c '{checkpoint, goal, steps: [.steps[] | {id, checkpoint}]}' "$V2_JOURNAL" >&2 || true
  operator_env "$OPERATOR_HOME/.local/bin/yard" update --check >&2 || true
}

for command in curl incus jq script sha256sum sudo systemctl timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
incus info >/dev/null || die 'initialized Incus is required'
incus image info "$CACHED_IMAGE" --project default >/dev/null \
  || die 'the P0 Debian image cache is required'

prepare_fixture
configure_published_agents
install -d "$RELEASE_ROOT"
cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
chmod -R a+rX "$RELEASE_ROOT"

info "installing exact published v$OLD_VERSION"
curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-all-errors --retry-delay 2 --connect-timeout 20 --max-time 300 \
  "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" -o "$INSTALLER"
[ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
  || die 'published installer checksum changed'
chmod 0755 "$INSTALLER"
operator_env env YARD_RELEASE_VERSION="$OLD_VERSION" "$INSTALLER" --version "$OLD_VERSION" --yes
[ "$(operator_env sha256sum \
  "$OPERATOR_HOME/.subyard/releases/$OLD_VERSION/subyard-$OLD_VERSION-linux-amd64.tar.gz" \
  | awk '{print $1}')" = "$OLD_BUNDLE_SHA256" ] || die 'published runtime archive checksum changed'

# Published init owns its project and later releases refuse a precreated one, so it
# stops once at the missing cached image and resumes once the project shares that image.
printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
if ! operator_yard init --yes >"$STATE_ROOT/init-first.log" 2>&1; then
  incus project show "$PROJECT" >/dev/null 2>&1 && [ -z "$(incus list --project "$PROJECT" --format csv -c n)" ] \
    || { cat "$STATE_ROOT/init-first.log" >&2; die 'first published init failed before its image step'; }
  incus project set "$PROJECT" features.images=false
  operator_yard init --yes
fi
incus project set "$PROJECT" user.p0-power-systemd="$MARKER"
operator_yard start --yes
[ "$(operator_yard --version)" = "yard $OLD_VERSION" ] || die 'published runtime is not active'
UUID_BEFORE="$(instance_uuid)"
guest sh -c 'printf "%s\n" "$1" > "$2"' _ "$MARKER" "$GUEST_MARKER"
[ "$(guest cat "$GUEST_MARKER")" = "$MARKER" ] || die 'guest marker was not written'
OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
write_fixture_value "$OLD_RELEASE_TARGET_STATE" "$OLD_RELEASE_TARGET"
assert_published_agent_configs
ok 'published runtime created a running yard'

if [ "$PUBLISHED_MODE" != direct ]; then
  assert_source_refusal
  ok 'update and --check refused without changing protected state'
  [ "$PUBLISHED_MODE" != refusal ] || exit 0
  # Chain catalog entries exist only for the first boundary release.
  operator_yard update --version "$ANCHOR_VERSION" --yes
  anchor_target="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  [ "$(operator_env sha256sum "$OPERATOR_HOME/.subyard/releases/$ANCHOR_VERSION/subyard-$ANCHOR_VERSION-linux-amd64.tar.gz" | awk '{print $1}')" = "$ANCHOR_SHA256" ] \
    || die 'anchor archive checksum changed'
  assert_ready "$ANCHOR_VERSION" "$anchor_target"
  # The actual old runtime is still previous here. Staged candidate tooling
  # must refuse that rollback without first activating the candidate.
  staged="$(operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --publish-only)"
  staged_path="$(sed -n 's/^staged release=.* candidate=//p' <<<"$staged")"
  case "$staged_path" in "$OPERATOR_HOME/.subyard/runtime/releases/$CANDIDATE_VERSION-"*) ;; *) die 'invalid staged candidate path' ;; esac
  anchor_settings="$(protected_settings_digest)"
  rollback_status=0
  operator_env "$staged_path/bin/yard" update --rollback --yes > "$STATE_ROOT/rollback-refusal.log" 2>&1 || rollback_status=$?
  [ "$rollback_status" != 0 ] && grep -Fq 'code=rollback-incompatible' "$STATE_ROOT/rollback-refusal.log" \
    && [ "$(protected_settings_digest)" = "$anchor_settings" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$anchor_target" ] \
    || die 'below-anchor rollback was not refused without mutation'
fi

info "updating to the frozen candidate $CANDIDATE_VERSION"
# The published launcher starts the update, as it does for an operator.
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" YARD_RELEASE_VERSION="$CANDIDATE_VERSION" \
  "$OPERATOR_HOME/.local/bin/yard" update --yes \
  || {
    report_failed_transition
    die 'candidate update failed'
  }
CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
write_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE" "$CANDIDATE_RELEASE_TARGET"
assert_ready
ok 'update reached Ready with the same instance and guest data'
