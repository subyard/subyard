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
# shellcheck source=dev/e2e/lib-p0-incus-image.sh
. "$ROOT/dev/e2e/lib-p0-incus-image.sh"
IFS=$'\t' read -r ANCHOR_VERSION _ _ _ < <(published_anchor)
# Below-anchor sources step through the hop, a fix release of the anchor series.
IFS=$'\t' read -r HOP_VERSION _ _ _ < <(published_hop)
[ "$VERSION" != anchor ] || VERSION="$ANCHOR_VERSION"
# The `hop` alias has no release catalog check; it remains for targeted and periodic runs.
[ "$VERSION" != hop ] || VERSION="$HOP_VERSION"
PUBLISHED_MODE=direct
case "${2:-}" in refusal) PUBLISHED_MODE="$2" ;; esac
# The catalog omits Codex for the v0.28.0 anchor because its anonymous GitHub REST
# metadata request can return HTTP 403. Later releases include the fix from #62.
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
[ "$#" -le 3 ] || { printf 'usage: published-upgrade.sh VERSION [AGENTS|all|none|refusal] [ROLLBACK_SCENARIO]\n' >&2; exit 2; }
# An owning profile's scenario runs on the candidate before the rollback and leaves the
# rollback admissible; the rollback, Ready and the update after it stay this lane's checks.
ROLLBACK_SCENARIO="${3:-}"
if [ -n "$ROLLBACK_SCENARIO" ]; then
  [ "$PUBLISHED_MODE" = direct ] && [[ "$ROLLBACK_SCENARIO" =~ ^config/profiles/[a-z0-9-]+/tests/e2e/[a-z0-9-]+\.sh$ ]] \
    && [ -f "$ROOT/$ROLLBACK_SCENARIO" ] && [ ! -L "$ROOT/$ROLLBACK_SCENARIO" ] \
    || { printf 'published-upgrade: invalid rollback scenario %q\n' "$ROLLBACK_SCENARIO" >&2; exit 2; }
fi
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

# Published init renders the config variant for its active approval prompts. The fixture
# leaves Git approval unset. Sudo also uses the published default except for the
# 0.30.0 grant scenario, which starts with a grant and no Codex sudo prompts.
published_agent_config_source() { # runtime agent source: prints the source published init renders
  operator_env python3 - "$1/config/agents/$2/integration.json" "$3" \
    "$OPERATOR_HOME/.config/subyard/config.env" <<'PY'
import json, pathlib, shlex, sys
integration, source, settings = sys.argv[1:]
integration, settings = pathlib.Path(integration), pathlib.Path(settings)
values = {}
for line in settings.read_text().splitlines():
    name, separator, value = line.partition("=")
    if separator and name in ("CODING_TOOL_GIT_APPROVAL", "DEV_SUDO"):
        values[name] = "".join(shlex.split(value))
gates = " ".join(gate for gate, active in (
    ("git", values.get("CODING_TOOL_GIT_APPROVAL", "") not in ("", "off")),
    ("sudo", values.get("DEV_SUDO", "") in ("", "prompt"))) if active)
if integration.exists():
    for artifact in json.loads(integration.read_text()).get("artifacts", []):
        if artifact.get("kind") == "config" and artifact.get("source") == source:
            source = artifact.get("approval_sources", {}).get(gates, source)
print(source)
PY
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
      claude) source=settings.json; destination=.claude/settings.json ;;
      codex) source=config.toml; destination=.codex/config.toml ;;
      opencode) source=opencode.jsonc; destination=.config/opencode/opencode.jsonc ;;
      pi) source=settings.json; destination=.pi/agent/settings.json ;;
    esac
    source="$(published_agent_config_source "$runtime" "$agent" "$source")" \
      || die "cannot read published $agent integration"
    expected="$(operator_env python3 -c "$digest_program" "$runtime/config/agents/$agent/$source")" \
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
platform=fixture-platform
if [[ "$PUBLISHED_AGENTS" =~ (^|[[:space:]])(claude|codex)($|[[:space:]]) ]]; then
  platform=fixture-platform-apparmor
fi
bash "$ROOT/dev/e2e/p0-guest.sh" "$platform" "$(date +%s)"

# Reuse its marked operator/project lifecycle, host-runtime snapshot and guarded cleanup.
set -- run "$TOKEN"
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"

IFS=$'\t' read -r OLD_VERSION OLD_INSTALLER_SHA256 OLD_BUNDLE_SHA256 EXPECTATION <<<"$PINS"
if [ "$PUBLISHED_MODE" = direct ]; then
  [[ "$EXPECTATION" = direct || "$EXPECTATION" = anchor || "$EXPECTATION" = hop ]] || die 'a refusal baseline cannot run a direct upgrade'
else
  [ "$EXPECTATION" = refusal ] || die 'refusal mode requires a refusal baseline'
fi
INSTALLER="$STATE_ROOT/subyard-install-v$OLD_VERSION.sh"
GUEST_MARKER=/var/tmp/subyard-published-upgrade-data
HOST_RULES_OVERRIDE="$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/rules/repo.rules"
HOST_RULES_DIGEST=''
# The 0.31.0 Codex lane also owns retained local configuration, offline activation
# repair and a symlinked instruction file across rollback; older releases lack its refusal.
LOCAL_CONFIG_CHECKS=0
if [ "$OLD_VERSION" = 0.31.0 ] && [[ " $PUBLISHED_AGENTS " = *" codex "* ]]; then LOCAL_CONFIG_CHECKS=1; fi
CACHED_IMAGE="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-subyard-e2e-debian-13-container}"

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
# Name the failing step and show its captured logs instead of exiting silently.
trap 'die "line $LINENO failed (exit $?)"' ERR

guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
instance_uuid() { incus config get "$INSTANCE" volatile.uuid --project "$PROJECT"; }

assert_ready() {
  local epochs guest_marker registry
  registry="$OPERATOR_HOME/.subyard/runtime/$CANDIDATE_RELEASE_TARGET/config/release-transition.json"
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'expected release is not active'
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
  operator_yard check --host >"$STATE_ROOT/check.log" 2>&1 || die 'yard check --host failed after update'
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
    grep -Fq "yard update --version $HOP_VERSION" "$STATE_ROOT/refusal-$arguments.stderr" \
      || die "$arguments lost the hop command (exit=$status)"
    [ "$(protected_settings_digest)" = "$settings" ] \
      && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "$before_links" ] \
      && [ "$(instance_uuid)" = "$UUID_BEFORE" ] \
      && [ "$(incus list --project "$PROJECT" --format csv -c s)" = RUNNING ] \
      && [ "$(guest cat "$GUEST_MARKER")" = "$MARKER" ] \
      || die 'refusal changed settings, runtime, instance, power or data'
  done
}

# The supported hop can retain schema-1 Git ownership from an earlier install.
# Derive valid source identities and digests with its own sync, then explicitly
# reopen that retained format; no unsupported release is installed.
prepare_retained_git_settings() {
  [ "$OLD_VERSION" = "$HOP_VERSION" ] || return 0
  RETAINED_SETTINGS_SOURCE="$(operator_env mktemp -d /tmp/subyard-published-settings.XXXXXX)"
  operator_env bash -se -- "$RETAINED_SETTINGS_SOURCE" <<'SH'
cd "$1"
install -d -m 0700 shared
printf '{"schemaVersion":1}\n' > subyard-config.json
printf "GIT_USER_NAME='Published Sync Author'\n" > shared/config.env
chmod 0600 subyard-config.json shared/config.env
git init -q
git add subyard-config.json shared
git -c user.name=Fixture -c user.email=fixture@example.invalid -c core.hooksPath=/dev/null commit -qm 'Configure retained published settings'
SH
  operator_yard config sync "$RETAINED_SETTINGS_SOURCE" --yes
  operator_yard init --yes
  operator_env python3 - "$OPERATOR_HOME/.config/subyard" <<'PY_SETTINGS'
import hashlib, json, pathlib
import sys
root = pathlib.Path(sys.argv[1])
manifest = root / '.sync/manifest.json'
value = json.loads(manifest.read_text())
assert value['schemaVersion'] == 2 and value['generation'] == 1
assert len(value['files']) == 1
file = value['files'][0]
assert file['path'] == '.sync/settings/overrides/shared/config.env'
cached = root / file['path']
assert hashlib.sha256(cached.read_bytes()).hexdigest() == file['digest']
assert cached.stat().st_mode & 0o777 == file['mode'] == 0o600
for directory in (root / 'overrides', root / 'overrides/shared'):
    directory.mkdir(mode=0o700, exist_ok=True)
    directory.chmod(0o700)
retained = root / 'overrides/shared/config.env'
assert not retained.exists() and not retained.is_symlink()
cached.rename(retained)
file['path'] = 'overrides/shared/config.env'
value['schemaVersion'] = 1
manifest.write_text(json.dumps(value) + '\n')
manifest.chmod(0o600)
PY_SETTINGS
  RETAINED_SETTINGS_DIGEST="$(operator_env sha256sum "$OPERATOR_HOME/.config/subyard/.sync/manifest.json" \
    "$OPERATOR_HOME/.config/subyard/overrides/shared/config.env")"
  RETAINED_SETTINGS_POWER="$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")"
  ok 'supported hop retains explicitly synthetic schema-1 Git settings'
}

migrate_retained_git_settings() {
  [ "$OLD_VERSION" = "$HOP_VERSION" ] || return 0
  [ "$(operator_env sha256sum "$OPERATOR_HOME/.config/subyard/.sync/manifest.json" \
    "$OPERATOR_HOME/.config/subyard/overrides/shared/config.env")" = "$RETAINED_SETTINGS_DIGEST" ] \
    || die 'candidate update changed retained schema-1 Git settings before sync'
  operator_yard config sync "$RETAINED_SETTINGS_SOURCE" --yes
  operator_env jq -e '.schemaVersion == 2 and .generation == 2 and (.files | length) == 1 and
    .files[0].path == ".sync/settings/overrides/shared/config.env"' \
    "$OPERATOR_HOME/.config/subyard/.sync/manifest.json" >/dev/null
  operator_env cmp "$RETAINED_SETTINGS_SOURCE/shared/config.env" \
    "$OPERATOR_HOME/.config/subyard/.sync/settings/overrides/shared/config.env"
  operator_env test ! -e "$OPERATOR_HOME/.config/subyard/overrides/shared/config.env"
  operator_env test ! -L "$OPERATOR_HOME/.config/subyard/overrides/shared/config.env"
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = "$RETAINED_SETTINGS_POWER" ] \
    || die 'settings migration changed the running yard power intent'
  assert_ready
  operator_env find "$RETAINED_SETTINGS_SOURCE" -depth -delete
  ok 'candidate sync migrated retained schema-1 settings without changing yard identity, power or data'
}

prepare_retained_codex_sudo_default() {
  guest python3 -c 'import pathlib,tomllib
policy=tomllib.loads(pathlib.Path("/etc/codex/requirements.toml").read_text())
assert not any(part.get("token") == "sudo" for rule in policy.get("rules",{}).get("prefix_rules",[]) for part in rule["pattern"])
for name in ("49-subyard-approval-hook.toml", "50-subyard-git-approval.rules.inc", "51-subyard-sudo-approval.rules.inc"):
    path=pathlib.Path("/etc/codex")/name
    assert not path.exists() and not path.is_symlink()'
  guest codex-policy-check >/dev/null || die 'published no-prompt Codex policy is not ready'
  guest runuser -u dev -- sudo -n true || die 'published no-prompt installation has no passwordless sudo grant'
  # The old Boolean grants sudo but does not control agent prompts or enter the
  # applied integration snapshot. Withdraw it so the candidate uses its default;
  # the running guest retains its real published policy and grant until update.
  operator_env sed -i '/^DEV_SUDO=1$/d' "$OPERATOR_HOME/.config/subyard/config.env"
  guest python3 -c 'import json,pathlib
value=json.loads(pathlib.Path("/var/lib/subyard/integrations/inventory.json").read_text())
assert "DEV_SUDO" not in value["applied_settings"]'
  operator_yard migrate --check --json >"$STATE_ROOT/no-prompt-ready.log" 2>&1 \
    && jq -e '(.outcome // .inspection.outcome).status == "ready"' "$STATE_ROOT/no-prompt-ready.log" >/dev/null \
    || die 'published no-prompt source does not report Ready'
  ok 'published retained default: running Codex has no sudo prompts, passwordless sudo works and source is Ready'
}

verify_combined_codex_policy_migration() {
  # Reopen the combined layout after the supported published release hands off
  # to the candidate. Its ordinary init must split the owned policy.
  local combined_policy combined_hash guest_hash
  combined_policy="$(mktemp /tmp/subyard-combined-codex-policy.XXXXXX)"
  cat > "$combined_policy" <<'REQUIREMENTS'
# Managed by Subyard Codex provision.
allowed_approval_policies = ["on-request"]
allowed_approvals_reviewers = ["user"]
check_for_update_on_startup = false
[rules]
prefix_rules = [
  { pattern = [{ token = "git" }, { any_of = ["commit", "push"] }], decision = "prompt" },
  { pattern = [{ token = "sudo" }], decision = "prompt" },
]
[features]
hooks = true
[hooks]
managed_dir = "/etc/codex"
[[hooks.PreToolUse]]
matcher = "Bash"
[[hooks.PreToolUse.hooks]]
type = "command"
command = "/etc/codex/subyard-git-approval-hook --plain git sudo"
timeout = 3
REQUIREMENTS
  # The bounded Incus wrapper closes stdin, so transfer the seed as a file.
  incus file push "$combined_policy" "$INSTANCE/etc/codex/requirements.toml" --project "$PROJECT"
  guest chmod 0644 /etc/codex/requirements.toml
  guest rm -f -- /etc/codex/49-subyard-approval-hook.toml \
    /etc/codex/50-subyard-git-approval.rules.inc /etc/codex/51-subyard-sudo-approval.rules.inc
  combined_hash="$(sha256sum "$combined_policy" | awk '{print $1}')"
  guest_hash="$(guest sha256sum /etc/codex/requirements.toml | awk '{print $1}')"
  [ "$guest_hash" = "$combined_hash" ] || die 'combined Codex policy seed was not installed'
  guest test ! -e /etc/codex/49-subyard-approval-hook.toml
  guest test ! -e /etc/codex/50-subyard-git-approval.rules.inc
  guest test ! -e /etc/codex/51-subyard-sudo-approval.rules.inc
  ok 'combined Codex policy seed installed: exact bytes and no split fragments before init'
  operator_yard init --yes || die 'init did not reprovision the combined Codex policy'
  guest grep -Fq '{ token = "sudo" }' /etc/codex/51-subyard-sudo-approval.rules.inc \
    || die 'init did not split the combined Codex policy'
  guest codex-policy-check >/dev/null \
    || die 'migrated Codex policies failed readiness'
  guest test ! -e /etc/codex/50-subyard-git-approval.rules.inc \
    || die 'sudo-only migration kept the Git approval policy'
  printf 'ok: ordinary yard reprovisioned the combined Codex policy\n'
}

guest_dev() { guest runuser -u dev -- "$@"; }

# An authored host file override of a shipped Codex rule is local configuration. Every
# update and rollback keeps its bytes, and all-local status/apply converge the yard to it.
seed_host_config_override() {
  operator_env install -d -m 0700 "$(dirname "$HOST_RULES_OVERRIDE")"
  operator_env bash -c 'printf "# published-upgrade host override\n" > "$1"; chmod 0600 "$1"' _ \
    "$HOST_RULES_OVERRIDE"
  HOST_RULES_DIGEST="$(operator_env sha256sum "$HOST_RULES_OVERRIDE" | awk '{print $1}')"
}

verify_config_workflow() { # <label>
  local status_rc=0
  operator_yard config status --all-local >"$STATE_ROOT/config-status.log" 2>&1 || status_rc=$?
  # Only materialized agent drift is expected; apply converges it.
  [ "$status_rc" = 0 ] || { [ "$status_rc" = 1 ] \
    && grep -Fq 'materialized agent config drift in yards:' "$STATE_ROOT/config-status.log"; } \
    || die "$1: config status failed for a reason other than agent drift"
  operator_yard config apply --all-local --yes >"$STATE_ROOT/config-apply.log" 2>&1 \
    || die "$1: config apply --all-local failed"
  operator_yard config status --all-local >"$STATE_ROOT/config-status.log" 2>&1 \
    || die "$1: config status reports drift after apply"
  [ "$(operator_env sha256sum "$HOST_RULES_OVERRIDE" | awk '{print $1}')" = "$HOST_RULES_DIGEST" ] \
    || die "$1: changed the host Codex rules override"
  [ "$(guest_dev sha256sum /home/dev/.codex/rules/repo.rules | awk '{print $1}')" = "$HOST_RULES_DIGEST" ] \
    || die "$1: the host Codex rules override is not applied to the yard"
  ok "$1: all-local config kept and applied the host override"
}

# Activation repair needs no release source: with every release input unusable, the
# current release reports and repairs a removed owned artifact without replaying
# migrations, selecting a release or changing the runtime pair.
verify_offline_activation_repair() {
  local report transaction target previous releases
  local -a offline=(env YARD_RELEASE_BASE_URL="file://$STATE_ROOT/forbidden-release"
    YARD_RELEASE_CACHE="$STATE_ROOT/forbidden-cache" YARD_RELEASE_VERSION=invalid/selected/version
    "$OPERATOR_HOME/.local/bin/yard")
  guest_dev rm /home/dev/.codex/rules/repo.rules
  operator_env cat "$V2_LEDGER" > "$STATE_ROOT/offline-ledger.before"
  operator_env cat "$V2_JOURNAL" > "$STATE_ROOT/offline-journal.before"
  transaction="$(operator_env jq -er '.transaction' "$V2_JOURNAL")"
  target="$(operator_env jq -er '.goal.target' "$V2_JOURNAL")"
  previous="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")"
  releases="$(operator_env ls -1 "$OPERATOR_HOME/.subyard/runtime/releases")"
  report="$(operator_env "${offline[@]}" migrate --check --json)" || die 'offline migrate --check failed'
  # shellcheck disable=SC2016 # jq binds $current itself.
  jq -e --arg current "$target" '
    .schemaVersion == 1 and .current == $current and .owner == $current and
    .outcome.status == "migration-required" and .next == "yard migrate" and
    all(.domains[]; .pending == []) and
    any(.decisions[]; .scope == "activation" and .resource == "activation.materialized-config")
  ' <<<"$report" >/dev/null || { printf '%s\n' "$report" >&2; die 'offline migrate --check did not identify the removed artifact'; }
  operator_env cat "$V2_JOURNAL" | cmp -s "$STATE_ROOT/offline-journal.before" - \
    || die 'offline migrate --check changed the journal'
  guest_dev test ! -e /home/dev/.codex/rules/repo.rules || die 'offline migrate --check repaired the artifact'
  operator_env "${offline[@]}" migrate --yes >"$STATE_ROOT/offline-migrate.log" 2>&1 \
    || die 'offline activation repair failed'
  operator_env cat "$V2_LEDGER" | cmp -s "$STATE_ROOT/offline-ledger.before" - \
    || die 'offline activation repair changed the migration ledger'
  # shellcheck disable=SC2016 # jq binds its arguments itself.
  operator_env jq -e --arg transaction "$transaction" --arg target "$target" '
    .goal == {target:$target,direction:"activate-target"} and .releases.target == $target and
    .transaction != $transaction and .steps == [] and .releases.from == $target
  ' "$V2_JOURNAL" >/dev/null || die 'offline activation repair replayed migrations or changed the target'
  [ "$(guest_dev sha256sum /home/dev/.codex/rules/repo.rules | awk '{print $1}')" = "$HOST_RULES_DIGEST" ] \
    || die 'offline activation repair did not restore the materialized rules'
  operator_env cat "$V2_JOURNAL" > "$STATE_ROOT/offline-journal.ready"
  operator_env "${offline[@]}" migrate </dev/null >"$STATE_ROOT/offline-migrate.log" 2>&1 \
    || die 'unconfirmed offline migrate failed on a ready release'
  operator_env cat "$V2_JOURNAL" | cmp -s "$STATE_ROOT/offline-journal.ready" - \
    && operator_env cat "$V2_LEDGER" | cmp -s "$STATE_ROOT/offline-ledger.before" - \
    || die 'ready offline migrate rewrote its journal or ledger'
  [ "$(operator_env ls -1 "$OPERATOR_HOME/.subyard/runtime/releases")" = "$releases" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "releases/$target" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")" = "$previous" ] \
    && [ ! -e "$STATE_ROOT/forbidden-cache" ] \
    || die 'offline migrate selected or published a release'
  assert_ready
  ok 'activation repair without any release source restored the removed artifact'
}

# A guest instruction file that only the runtime refreshes, replaced by a symlink in the
# yard: the rollback, or init after it, refuses it by name with the operator's step, keeps
# the link and its target, and converges once the operator removes it.
verify_symlinked_instruction_file() { # <rollback command...>
  local link=/home/dev/.codex/AGENTS.md output rc=0
  local step='replace it with a regular file or remove it, then run yard -Y default init --configs'
  local preserve="'mv' '-T' '--backup=numbered' '--' '$link'"
  operator_env install -d -m 0700 "$OPERATOR_HOME/.codex"
  printf 'published-upgrade instructions\n' | operator_env tee "$OPERATOR_HOME/.codex/AGENTS.md" >/dev/null
  operator_yard init --yes >"$STATE_ROOT/instructions-init.log" 2>&1 || die 'init did not refresh the instruction file'
  guest_dev test -f "$link" -a ! -L "$link" || die 'the instruction file was not refreshed as a regular file'
  guest_dev sh -c 'mv "$1" "$1.real" && ln -s AGENTS.md.real "$1"' _ "$link"
  output="$("$@" 2>&1)" || rc=$?
  printf '%s\n' "$output" > "$STATE_ROOT/published-rollback.log"
  if [ "$rc" -eq 0 ]; then
    output="$(operator_yard init --yes 2>&1)" && die 'init replaced the symlinked instruction file'
    printf '%s\n' "$output" >> "$STATE_ROOT/published-rollback.log"
  fi
  { grep -Fq "$link is not a readable regular file" <<<"$output" && grep -Fq "$step" <<<"$output"; } \
    || grep -Fq "$preserve" <<<"$output" \
    || die 'the symlinked instruction file was refused without its path and step'
  guest_dev test -L "$link" && [ "$(guest_dev cat "$link.real")" = 'published-upgrade instructions' ] \
    || die 'the refusal changed the symlinked instruction file or its target'
  guest_dev rm "$link" "$link.real"
  # A refused rollback first repairs the current release, then runs again.
  [ "$rc" -eq 0 ] || operator_yard migrate --yes >"$STATE_ROOT/instructions-migrate.log" 2>&1 \
    || die 'migrate after the refused rollback failed'
  operator_yard init --yes >"$STATE_ROOT/instructions-init.log" 2>&1 || die 'init after removing the link failed'
  guest_dev test -f "$link" -a ! -L "$link" && guest_dev grep -Fqx 'published-upgrade instructions' "$link" \
    || die 'the replaced instruction file did not converge'
  [ "$rc" -eq 0 ] || "$@" >"$STATE_ROOT/published-rollback.log" 2>&1 || die 'rollback after removing the link failed'
  ok "symlinked instruction file refused by name and kept (rollback exit $rc), then converged"
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
image_reference="$(p0_container_image_reference "$CACHED_IMAGE")" || exit $?

prepare_fixture
configure_published_agents
if [ "$OLD_VERSION" = 0.30.0 ] && [[ " $PUBLISHED_AGENTS " = *" codex "* ]]; then
  # Use the fixed published provisioner to create the retained no-prompt state.
  operator_env sh -c 'printf "%s\n" DEV_SUDO=1 >> "$1"' _ "$OPERATOR_HOME/.config/subyard/config.env"
fi
# Published init creates its own project. Resolve the cached image through its public remote
# so both the old and new runtime can use it within the private project image namespace.
operator_env sed -i -E "s/^(BASE_IMAGE|BASE_IMAGE_FALLBACK)=.*/\1=$image_reference/" \
  "$OPERATOR_HOME/.config/subyard/config.env"
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

printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
operator_yard init --yes
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
if [ "$LOCAL_CONFIG_CHECKS" = 1 ]; then
  seed_host_config_override
  verify_config_workflow 'published release'
fi
if [ "$OLD_VERSION" = 0.30.0 ] && [[ " $PUBLISHED_AGENTS " = *" codex "* ]]; then
  prepare_retained_codex_sudo_default
fi
prepare_retained_git_settings

if [ "$PUBLISHED_MODE" != direct ]; then
  assert_source_refusal
  ok 'update and --check refused without changing protected state'
  exit 0
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
if [ "$OLD_VERSION" = 0.30.0 ] && [[ " $PUBLISHED_AGENTS " = *" codex "* ]]; then
  operator_yard config show DEV_SUDO | grep -Fx 'effective: prompt' >/dev/null \
    || die 'candidate does not default DEV_SUDO to prompt'
  guest grep -Fq '{ token = "sudo" }' /etc/codex/51-subyard-sudo-approval.rules.inc \
    && guest codex-policy-check >/dev/null \
    || die 'candidate activation did not install default Codex sudo prompts'
  ok 'candidate activation installed default Codex sudo prompts before ordinary init'
  guest runuser -u dev -- sudo -n true || die 'candidate activation removed the existing passwordless sudo grant'
  ok 'candidate activation preserved the existing passwordless sudo grant'
fi
assert_ready
migrate_retained_git_settings
case " $PUBLISHED_AGENTS " in *" claude "*)
  [ "$(guest bash -lc 'echo "$CLAUDE_CODE_DISABLE_INLINE_SHELL_RM_PROMPT:$CLAUDE_CODE_DISABLE_SUBSTITUTION_RM_PROMPT"')" = 1:1 ] \
    || die 'updated Claude login-shell environment is missing'
  operator_yard shell -- env > "$STATE_ROOT/shell-environment"
  grep -Fxq 'CLAUDE_CODE_DISABLE_INLINE_SHELL_RM_PROMPT=1' "$STATE_ROOT/shell-environment"
  grep -Fxq 'CLAUDE_CODE_DISABLE_SUBSTITUTION_RM_PROMPT=1' "$STATE_ROOT/shell-environment"
  ;;
esac
if [ "$LOCAL_CONFIG_CHECKS" = 1 ]; then
  verify_combined_codex_policy_migration
  assert_ready
  verify_config_workflow 'candidate update'
  verify_offline_activation_repair
fi
ok 'update reached Ready with the same instance and guest data'

# A published release that ships the candidate's exact registry stays its rollback target.
runtime="$OPERATOR_HOME/.subyard/runtime"
registry_relation=0
operator_env cmp -s "$runtime/$OLD_RELEASE_TARGET/config/release-transition.json" \
  "$runtime/$CANDIDATE_RELEASE_TARGET/config/release-transition.json" || registry_relation=$?
# cmp exits 1 for different registries and 2 when either cannot be read.
[ "$registry_relation" -le 1 ] || die 'cannot compare the published and candidate registries'
[ -z "$ROLLBACK_SCENARIO" ] || [ "$registry_relation" = 0 ] || die 'the rollback scenario needs a rollback target'
if [ "$registry_relation" = 0 ]; then
  if [ -n "$ROLLBACK_SCENARIO" ]; then
    info "running rollback scenario $ROLLBACK_SCENARIO"
    # shellcheck disable=SC1090
    . "$ROOT/$ROLLBACK_SCENARIO"
  fi
  history_before="$(operator_env sha256sum "$V2_STATE_ROOT/history-checkpoint.json" | awk '{print $1}')"
  info "rolling back to the published v$OLD_VERSION"
  if [ "$LOCAL_CONFIG_CHECKS" = 1 ]; then
    verify_symlinked_instruction_file operator_yard update --rollback --yes
  else
    operator_yard update --rollback --yes > "$STATE_ROOT/published-rollback.log" 2>&1 \
      || { cat "$STATE_ROOT/published-rollback.log" >&2; die 'rollback to the published release failed'; }
  fi
  [ "$(operator_env readlink "$runtime/current")" = "$OLD_RELEASE_TARGET" ] \
    && [ "$(operator_env readlink "$runtime/previous")" = "$CANDIDATE_RELEASE_TARGET" ] \
    && [ "$(operator_yard --version)" = "yard $OLD_VERSION" ] \
    || die 'rollback did not activate the published release'
  [ "$(operator_env sha256sum "$V2_STATE_ROOT/history-checkpoint.json" | awk '{print $1}')" = "$history_before" ] \
    || die 'rollback changed the migration checkpoint'
  operator_yard migrate --check --json >"$STATE_ROOT/rollback-migrate-check.log" 2>&1 \
    && jq -e '(.outcome // .inspection.outcome).status == "ready"' "$STATE_ROOT/rollback-migrate-check.log" >/dev/null \
    || die 'published release does not report Ready after rollback'
  ok 'rollback to the published release kept migration history'
  [ "$LOCAL_CONFIG_CHECKS" = 0 ] || verify_config_workflow 'rollback'
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" YARD_RELEASE_VERSION="$CANDIDATE_VERSION" \
    "$OPERATOR_HOME/.local/bin/yard" update --yes \
    || { report_failed_transition; die 'update after rollback failed'; }
  [ "$(operator_env readlink "$runtime/current")" = "$CANDIDATE_RELEASE_TARGET" ] \
    || die 'update after rollback did not reactivate the candidate'
  assert_ready
  [ "$LOCAL_CONFIG_CHECKS" = 0 ] || verify_config_workflow 'update after rollback'
  ok 'update after rollback reached Ready again'
fi
