#!/usr/bin/env bash
# Pinned v0.14.0 default integrations -> candidate ownership adoption on one allocated VM.
set -Eeuo pipefail
umask 022

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_ID="${SUBYARD_E2E_RUN_ID:-}"
[[ "$RUN_ID" =~ ^[0-9a-f]{8}$ ]] \
  || { printf 'integration-legacy-upgrade: run through dev/agent-e2e.sh\n' >&2; exit 2; }
TOKEN=$((16#$RUN_ID))

# Reuse its marked operator/project lifecycle, host-runtime snapshot and guarded cleanup.
set -- run "$TOKEN"
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"

OLD_VERSION=0.14.0
OLD_INSTALLER_SHA256=a78c10910c0886a8d01c0a2e77718b21ce58e45fa7f0de6064bc1f351d56028b
CANDIDATE_VERSION="0.14.1-legacy.upgrade.$TOKEN"
INSTALLER="$STATE_ROOT/subyard-install-v0.14.0.sh"
HOST_CONFIG="$OPERATOR_HOME/.config/subyard/config.env"
YARD_CONFIG="$OPERATOR_HOME/.config/subyard/yards/default/config.env"
INVENTORY=/var/lib/subyard/integrations/inventory.json
PLAIN_PATH=/home/dev/.codex/rules/repo.rules
LINK_PATH=/home/dev/.claude/projects
LINK_TARGET=/mnt/host/agent-sessions/claude/projects
HISTORY_PATH="$LINK_TARGET/legacy-upgrade-history"
CANDIDATE_LAUNCHER=''
HOST_CONFIG_FINGERPRINT=''

die() {
  printf 'integration-legacy-upgrade: %s\n' "$*" >&2
  local log
  for log in "$STATE_ROOT"/*.log; do
    [ ! -f "$log" ] || tail -n 40 "$log" >&2
  done
  exit 2
}
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

for command in cmp curl go incus jq script sha256sum sudo systemctl timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
incus info >/dev/null || die 'initialized Incus is required'

guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }

legacy_fingerprint() {
  guest sh -eu -c '
    sha256sum \
      /home/dev/.claude/settings.json \
      /home/dev/.codex/config.toml \
      /home/dev/.codex/rules/repo.rules \
      /home/dev/.config/opencode/opencode.jsonc \
      /home/dev/.pi/agent/settings.json \
      /home/dev/.claude/auth.json \
      "$1"
    for path in \
      /home/dev/.claude/projects \
      /home/dev/.claude-memory \
      /home/dev/.codex/sessions \
      /home/dev/.local/share/opencode/opencode.db \
      /home/dev/.pi/agent/sessions; do
      printf "%s|" "$path"
      readlink "$path"
      stat -c "%u:%g" "$path"
    done
  ' subyard "$HISTORY_PATH" | sha256sum | awk '{print $1}'
}

assert_old_active() {
  [ "$(operator_yard --version)" = "yard $OLD_VERSION" ] \
    || die "active runtime changed from yard $OLD_VERSION"
}

assert_unadopted() {
  local expected="$1"
  assert_old_active
  operator_env test ! -e "$YARD_CONFIG" \
    || die 'candidate wrote the default-yard selection before adoption'
  [ "$(operator_env sha256sum "$HOST_CONFIG" | awk '{print $1}')" = \
    "$HOST_CONFIG_FINGERPRINT" ] || die 'candidate changed host settings before adoption'
  guest test ! -e "$INVENTORY" \
    || die 'candidate wrote integration inventory before adoption'
  [ "$(legacy_fingerprint)" = "$expected" ] \
    || die 'read-only or refused adoption changed legacy integration state'
}

run_declined() {
  local output="$1" release_base="$2" command=''
  shift 2
  printf -v command '%q ' "$@"
  set +e
  operator_env env YARD_RELEASE_BASE_URL="$release_base" bash -c '
    printf "n\n" | script -qefc "$1" /dev/null
  ' _ "$command" >"$output" 2>&1
  local rc=$?
  set -e
  [ "$rc" -ne 0 ] || die "declined command succeeded: $command"
  grep -Fq 'operation declined' "$output" \
    || die "declined command returned the wrong diagnostic: $command"
}

assert_adoption_plan() {
  local output="$1" path
  for path in \
    /home/dev/.claude/settings.json \
    /home/dev/.codex/rules/repo.rules \
    /home/dev/.claude/projects; do
    grep -Fq "$path" "$output" || die "adoption plan omitted $path"
  done
}

find_candidate_launcher() {
  CANDIDATE_LAUNCHER="$(operator_env bash -c '
    shopt -s nullglob
    matches=()
    for launcher in "$1"/releases/*/bin/yard; do
      [ -x "$launcher" ] || continue
      [ "$("$launcher" --version)" = "yard $2" ] || continue
      matches+=("$launcher")
    done
    [ "${#matches[@]}" = 1 ] || exit 1
    printf "%s\n" "${matches[0]}"
  ' _ "$OPERATOR_HOME/.subyard/runtime" "$CANDIDATE_VERSION")" \
    || die 'could not identify the one verified published candidate launcher'
}

assert_preserved_user_state() {
  guest sh -eu -c '
    [ "$(cat /home/dev/.claude/auth.json)" = legacy-auth ]
    [ "$(cat "$1")" = legacy-history ]
    python3 -c '\''import json
p = "/home/dev/.claude/settings.json"
value = json.load(open(p))
assert value["fixtureUnmanaged"] == {"marker": "legacy-upgrade"}'\''
  ' subyard "$HISTORY_PATH" || die 'Claude auth, history or unmanaged settings were not preserved'
}

prepare_fixture
p0_capacity_reset_build_cache

# The shared helper deliberately prepares AGENTS=none for its own fixture. Remove it entirely:
# v0.14.0 must resolve its ordinary shipped five-tool default.
operator_env bash -c 'cat > "$1" <<EOF
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
BASE_IMAGE_FALLBACK=images:debian/13/cloud
DEV_UID=2000
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
EOF
chmod 0600 "$1"' _ "$HOST_CONFIG"

info "packaging candidate $CANDIDATE_VERSION"
"$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" \
  --version "$CANDIDATE_VERSION" >/dev/null
chmod -R a+rX "$RELEASE_ROOT"

info "installing checksum-pinned published baseline $OLD_VERSION"
curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
  "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" \
  -o "$INSTALLER"
[ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
  || die 'published v0.14.0 installer checksum changed'
chmod 0755 "$INSTALLER"
operator_env "$INSTALLER" --version "$OLD_VERSION" --yes
printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
incus project create "$PROJECT" \
  -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
operator_yard init --yes
operator_yard start --yes
assert_old_active

guest sh -eu -c '
  command -v codex >/dev/null
  command -v opencode >/dev/null
  command -v ai-observer >/dev/null
  test -f /home/dev/.claude/settings.json
  test -f /home/dev/.pi/agent/settings.json
  for path in \
    /home/dev/.claude/projects \
    /home/dev/.claude-memory \
    /home/dev/.codex/sessions \
    /home/dev/.local/share/opencode/opencode.db \
    /home/dev/.pi/agent/sessions; do test -L "$path"; done
' || die 'published v0.14.0 did not install the ordinary five integrations'
operator_env test ! -e "$YARD_CONFIG" \
  || die 'published baseline unexpectedly has canonical default-yard settings'
guest test ! -e "$INVENTORY" \
  || die 'published baseline unexpectedly has integration inventory'

guest sh -eu -c '
  printf "%s\n" legacy-auth > /home/dev/.claude/auth.json
  printf "%s\n" legacy-history > "$1"
  python3 -c '\''import json
p = "/home/dev/.claude/settings.json"
value = json.load(open(p))
value["fixtureUnmanaged"] = {"marker": "legacy-upgrade"}
open(p, "w").write(json.dumps(value, indent=2) + "\n")'\''
' subyard "$HISTORY_PATH"
BASELINE_FINGERPRINT="$(legacy_fingerprint)"
HOST_CONFIG_FINGERPRINT="$(operator_env sha256sum "$HOST_CONFIG" | awk '{print $1}')"

info 'declining the candidate-owned update plan before any integration adoption'
run_declined "$STATE_ROOT/update-declined.log" "file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION"
assert_adoption_plan "$STATE_ROOT/update-declined.log"
assert_unadopted "$BASELINE_FINGERPRINT"
find_candidate_launcher

info 'declining the explicit candidate init adoption plan'
run_declined "$STATE_ROOT/init-declined.log" "file://$RELEASE_ROOT" \
  "$CANDIDATE_LAUNCHER" init
assert_adoption_plan "$STATE_ROOT/init-declined.log"
assert_unadopted "$BASELINE_FINGERPRINT"

info 'rejecting a changed legacy plain file without claiming or replacing it'
guest sh -eu -c 'cp "$1" /tmp/subyard-legacy-rules; printf "%s\n" changed-legacy-rule >> "$1"' \
  subyard "$PLAIN_PATH"
PLAIN_DRIFT_FINGERPRINT="$(legacy_fingerprint)"
set +e
operator_env "$CANDIDATE_LAUNCHER" init --yes >"$STATE_ROOT/plain-drift.log" 2>&1
plain_rc=$?
set -e
[ "$plain_rc" -ne 0 ] || die 'candidate init adopted a changed legacy plain file'
grep -Fq "$PLAIN_PATH" "$STATE_ROOT/plain-drift.log" \
  || die 'plain-file refusal omitted the conflicting path'
assert_unadopted "$PLAIN_DRIFT_FINGERPRINT"
guest sh -eu -c 'cat /tmp/subyard-legacy-rules > "$1"' subyard "$PLAIN_PATH"
[ "$(legacy_fingerprint)" = "$BASELINE_FINGERPRINT" ] \
  || die 'plain-file fixture did not restore the exact legacy state'

info 'rejecting a changed legacy link without claiming or replacing it'
guest sh -eu -c '
  ln -sfn /mnt/host/agent-sessions/claude/memory "$1"
  chown -h 2000:2000 "$1"
' subyard "$LINK_PATH"
LINK_DRIFT_FINGERPRINT="$(legacy_fingerprint)"
set +e
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes \
  >"$STATE_ROOT/link-drift.log" 2>&1
link_rc=$?
set -e
[ "$link_rc" -ne 0 ] || die 'published updater adopted a changed legacy link'
assert_unadopted "$LINK_DRIFT_FINGERPRINT"
guest sh -eu -c '
  ln -sfn "$2" "$1"
  chown -h 2000:2000 "$1"
' subyard "$LINK_PATH" "$LINK_TARGET"
[ "$(legacy_fingerprint)" = "$BASELINE_FINGERPRINT" ] \
  || die 'link fixture did not restore the exact legacy state'

info 'recovering an unselected legacy Paseo service through the downloaded candidate hint'
# Explicit persistent selection authorizes cleanup without adopting integration inventory.
operator_env bash -c 'printf "AGENTS=claude codex opencode pi aiobserver\n" >> "$1"' _ "$HOST_CONFIG"
HOST_CONFIG_FINGERPRINT="$(operator_env sha256sum "$HOST_CONFIG" | awk '{print $1}')"
guest sh -eu -c '
  [ ! -e /var/lib/subyard/paseo-ownership ]
  [ ! -e /etc/systemd/system/paseo.service ]
  [ ! -e /etc/systemd/system/paseo.service.subyard-retired ]
  install -d /srv/agents/paseo/data
  printf "%s\n" legacy-paseo-data > /srv/agents/paseo/data/cleanup-sentinel
  printf "%s\n" "[Service]" "ExecStart=/usr/bin/sleep infinity" \
    "[Install]" "WantedBy=multi-user.target" > /etc/systemd/system/paseo.service
  chmod 0644 /etc/systemd/system/paseo.service
  systemctl daemon-reload
  systemctl enable --now paseo.service
'
paseo_before="$(guest sha256sum /etc/systemd/system/paseo.service /srv/agents/paseo/data/cleanup-sentinel)"
if operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes \
  >"$STATE_ROOT/paseo-update-blocked.log" 2>&1; then
  die 'published updater accepted an unowned unselected Paseo service'
fi
assert_unadopted "$BASELINE_FINGERPRINT"
# Parse the printed command as argv and validate the exact candidate and bounded action.
# Never evaluate diagnostic text as shell code.
python3 - "$STATE_ROOT/paseo-update-blocked.log" "${CANDIDATE_LAUNCHER%/yard}/yard-engine" \
  >"$STATE_ROOT/cleanup-command" <<'PYHINT'
import shlex
import sys
lines = open(sys.argv[1]).read().splitlines()
expected = [sys.argv[2], "-Y", "default", "integration", "cleanup", "paseo"]
commands = [shlex.split(line) for line in lines if "'integration' 'cleanup' 'paseo'" in line]
assert expected in commands and expected + ["--check"] in commands, commands
sys.stdout.buffer.write(b"\0".join(part.encode() for part in expected) + b"\0")
PYHINT
mapfile -d '' -t cleanup_command < "$STATE_ROOT/cleanup-command"
operator_env "${cleanup_command[@]}" --check >"$STATE_ROOT/paseo-cleanup-check.log"
if operator_env "${cleanup_command[@]}" </dev/null >"$STATE_ROOT/paseo-cleanup-eof.log" 2>&1; then
  die 'EOF accepted candidate cleanup'
fi
[ "$(guest sha256sum /etc/systemd/system/paseo.service /srv/agents/paseo/data/cleanup-sentinel)" = "$paseo_before" ] \
  || die 'candidate cleanup preview or EOF changed preserved artifacts'
guest sh -eu -c '
  systemctl is-active --quiet paseo.service
  systemctl is-enabled --quiet paseo.service
  [ ! -e /etc/systemd/system/paseo.service.subyard-retired ]
  [ ! -e /var/lib/subyard/paseo-ownership ]
'
assert_unadopted "$BASELINE_FINGERPRINT"
operator_env "${cleanup_command[@]}" --yes >"$STATE_ROOT/paseo-cleanup-apply.log"
guest sh -eu -c '
  [ ! -e /etc/systemd/system/paseo.service ]
  [ -f /etc/systemd/system/paseo.service.subyard-retired ]
  [ ! -e /var/lib/subyard/paseo-ownership ]
  [ "$(cat /srv/agents/paseo/data/cleanup-sentinel)" = legacy-paseo-data ]
  ! systemctl is-active --quiet paseo.service
  ! systemctl is-enabled --quiet paseo.service
'
[ "$(guest sha256sum /etc/systemd/system/paseo.service.subyard-retired | awk '{print $1}')" = \
  "$(printf '%s\n' "$paseo_before" | head -n 1 | awk '{print $1}')" ] \
  || die 'candidate cleanup changed the saved unit'
paseo_retired="$(guest sha256sum /etc/systemd/system/paseo.service.subyard-retired /srv/agents/paseo/data/cleanup-sentinel)"
operator_env "${cleanup_command[@]}" --yes >"$STATE_ROOT/paseo-cleanup-retry.log"
grep -Fq 'No cleanup needed.' "$STATE_ROOT/paseo-cleanup-retry.log" \
  || die 'candidate cleanup retry was not a no-op'
[ "$(guest sha256sum /etc/systemd/system/paseo.service.subyard-retired /srv/agents/paseo/data/cleanup-sentinel)" = "$paseo_retired" ] \
  || die 'candidate cleanup retry changed preserved artifacts'
assert_unadopted "$BASELINE_FINGERPRINT"

info 'upgrading through the published updater and adopting the exact legacy state'
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
[ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] \
  || die 'candidate release was not activated'
guest test -f "$INVENTORY" || die 'successful upgrade did not publish integration inventory'
operator_yard integration status --json > "$STATE_ROOT/adopted-status.json"
jq -e '
  .observed == "ready" and
  .selection.requested == ["claude","codex","opencode","pi","aiobserver"]
' "$STATE_ROOT/adopted-status.json" >/dev/null \
  || die 'successful upgrade did not adopt the ordinary five integrations'
assert_preserved_user_state

info 'materializing the canonical per-yard selection through explicit init'
operator_yard init --yes
operator_env grep -Eq '^CODING_TOOL_INTEGRATIONS=' "$YARD_CONFIG" \
  || die 'explicit candidate init did not persist the default requested set'
operator_yard integration status --json | jq -e '
  .observed == "ready" and
  .selection.requested == ["claude","codex","opencode","pi","aiobserver"]
' >/dev/null || die 'explicit candidate init changed the adopted requested set'
assert_preserved_user_state

info 'repeating explicit init without rewriting stable integration evidence'
inventory_before="$(guest sha256sum "$INVENTORY")"
config_before="$(operator_env sha256sum "$YARD_CONFIG")"
state_before="$(legacy_fingerprint)"
operator_yard init --yes
[ "$(guest sha256sum "$INVENTORY")" = "$inventory_before" ] \
  || die 'repeated init rewrote stable integration inventory'
[ "$(operator_env sha256sum "$YARD_CONFIG")" = "$config_before" ] \
  || die 'repeated init rewrote stable requested settings'
[ "$(legacy_fingerprint)" = "$state_before" ] \
  || die 'repeated init changed stable integration artifacts'

info 'disabling and re-enabling Claude while preserving user-owned state'
operator_yard integration disable claude --yes
operator_yard integration status --json > "$STATE_ROOT/disabled-status.json"
jq -e '
  .observed == "ready" and (.selection.requested | length) == 4 and
  (.selection.requested | index("claude") | not)
' "$STATE_ROOT/disabled-status.json" >/dev/null \
  || die 'Claude disable did not converge to the four-tool requested set'
guest test ! -L "$LINK_PATH" || die 'Claude disable retained the owned history link'
assert_preserved_user_state

operator_yard integration enable claude --yes
operator_yard integration status --json > "$STATE_ROOT/re-enabled-status.json"
jq -e '
  .observed == "ready" and (.selection.requested | length) == 5 and
  (.selection.requested | index("claude") != null)
' "$STATE_ROOT/re-enabled-status.json" >/dev/null \
  || die 'Claude re-enable did not converge to the five-tool requested set'
[ "$(guest readlink "$LINK_PATH")" = "$LINK_TARGET" ] \
  || die 'Claude re-enable did not restore the owned history link'
assert_preserved_user_state

info 'rolling back to the unmodified v0.14.0 reader and refreshing its configs'
operator_yard update --rollback --yes
assert_old_active
operator_yard init --configs --yes
assert_preserved_user_state

info 'retrying the candidate upgrade after retained-runtime config writes'
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
operator_yard integration status --json > "$STATE_ROOT/re-upgraded-status.json"
jq -e '.observed == "ready" and (.selection.requested | length) == 5' \
  "$STATE_ROOT/re-upgraded-status.json" >/dev/null \
  || die 'candidate did not recover after retained-runtime config writes'
assert_preserved_user_state

ok 'pinned v0.14.0 adoption, refusal, decline, lifecycle preservation, rollback and forward retry passed'
