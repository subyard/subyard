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
OPENCODE_CONFIG="$OPERATOR_HOME/opencode-fixture.jsonc"
OPENCODE_INSTRUCTIONS="$OPERATOR_HOME/opencode-instructions.md"
CODEX_RULES="$OPERATOR_HOME/codex-rules-fixture.rules"

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
      /usr/local/libexec/subyard/projects-changed \
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
  if ! grep -Fq 'operation declined' "$output"; then
    # Activation hides private adapter errors; the public read-only status
    # exposes its bounded diagnostic without changing the original failure.
    if find_candidate_launcher; then
      operator_env timeout 120 "$CANDIDATE_LAUNCHER" -Y default integration status --json \
        >"$output.integration-status.log" 2>&1 || true
    else
      printf '%s\n' 'Verified candidate unavailable for read-only status diagnostic.' \
        >"$output.integration-status.log"
    fi
    die "declined command returned the wrong diagnostic: $command"
  fi
}

assert_adoption_plan() {
  local output="$1" path
  for path in \
    /home/dev/.claude/settings.json \
    /home/dev/.codex/rules/repo.rules \
    /home/dev/.claude/projects; do
    if ! grep -Fq "$path" "$output"; then
      cat "$output" >&2 || true
      if find_candidate_launcher; then
        operator_env timeout 120 "$CANDIDATE_LAUNCHER" -Y default integration status --json \
          >"$output.integration-status.log" 2>&1 || true
      fi
      die "adoption plan omitted $path"
    fi
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
  ' _ "$OPERATOR_HOME/.subyard/runtime" "$CANDIDATE_VERSION")"
}

yard_config_fingerprint() {
  operator_env sh -c '
    if [ -e "$1" ]; then sha256sum "$1" | { read -r digest _; printf "%s\n" "$digest"; }; else printf absent; fi
  ' _ "$YARD_CONFIG"
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
# This integration-only fixture explicitly selects no optional environment profiles.
# The controller opens this public asset: the fixture operator cannot traverse
# the private transported checkout. Fail before baseline setup on any copy error.
operator_env bash -eu -c 'cat > "$1" <<EOF
ENVIRONMENT_PROFILES=
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
BASE_IMAGE_FALLBACK=images:debian/13/cloud
DEV_UID=2000
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=$3
AGENT_opencode_CONFIG=$2
EOF
chmod 0600 "$1"
printf "%s\n" "// artifact declaration cutover preserves raw JSONC" > "$2"
cat >> "$2"
printf "%s\n" "OpenCode operator instructions survive artifact cutover." > "$3"
chmod 0600 "$2" "$3"' _ "$HOST_CONFIG" "$OPENCODE_CONFIG" "$OPENCODE_INSTRUCTIONS" \
  < "$ROOT/config/agents/opencode/opencode.jsonc"

info "packaging candidate $CANDIDATE_VERSION"
# Synthetic declarations and hooks exercise the shared native contract without
# installing a shipped profile's external toolchain. The predecessor stays exact.
for fixture_profile in lifecycle-native lifecycle-other; do
  install -d -m 0755 "$ROOT/config/profiles/$fixture_profile"
  printf 'PROFILE_NAME=%s\n' "$fixture_profile" > "$ROOT/config/profiles/$fixture_profile/profile.conf"
  cat > "$ROOT/config/profiles/$fixture_profile/provision.sh" <<'PROFILE_HOOK'
#!/bin/sh
# subyard-provision-check-v1
set -eu
marker="/srv/subyard-PROFILE_FIXTURE-provisioned"
for argument in "$@"; do
  case "$argument" in --check) [ -f "$marker" ] && exit 0; exit 10 ;; --yes) ;; *) exit 2 ;; esac
done
printf 'synthetic provision hook\n' > "$marker"
PROFILE_HOOK
  sed -i "s/PROFILE_FIXTURE/$fixture_profile/" "$ROOT/config/profiles/$fixture_profile/provision.sh"
  chmod 0755 "$ROOT/config/profiles/$fixture_profile/provision.sh"
done
printf 'YARD_DEVICES=kvm\nYARD_CAPS=host-memory\n' >> "$ROOT/config/profiles/lifecycle-native/profile.conf"
sha256sum "$ROOT/config/profiles/"lifecycle-*/{profile.conf,provision.sh}
"$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" \
  --version "$CANDIDATE_VERSION" >/dev/null
sha256sum "$RELEASE_ROOT"/subyard-*.tar.gz
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
OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
case "$OLD_RELEASE_TARGET" in
  releases/"$OLD_VERSION"-*) ;;
  *) die 'published baseline runtime target does not identify the predecessor' ;;
esac
[[ "${OLD_RELEASE_TARGET#releases/"$OLD_VERSION"-}" =~ ^[0-9a-f]{12}$ ]] \
  || die 'published baseline runtime target has an invalid artifact identity'

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

# Keep the authentic predecessor bytes as an explicit operator input. Newer
# shipped Codex rules are a different desired artifact, so strict initial
# adoption correctly refuses them rather than guessing ownership of old bytes.
guest stat -c 'legacy-codex-rules mode=%a uid=%u gid=%g links=%h' "$PLAIN_PATH"
guest sha256sum "$PLAIN_PATH"
[ "$(guest stat -c '%a:%u:%g:%h' "$PLAIN_PATH")" = '644:2000:2000:1' ] \
  || die 'authentic Codex rules have unexpected native ownership'
guest cat "$PLAIN_PATH" | operator_env sh -eu -c '
  cat > "$1"
  chmod 0600 "$1"
  printf "\nAGENT_codex_RULES=%s\n" "$1" >> "$2"
' subyard "$CODEX_RULES" "$HOST_CONFIG"
[ "$(guest sha256sum "$PLAIN_PATH" | awk '{print $1}')" = \
  "$(operator_env sha256sum "$CODEX_RULES" | awk '{print $1}')" ] \
  || die 'explicit Codex rules source differs from authentic predecessor bytes'

guest sh -eu -c '
  printf "%s\n" legacy-auth > /home/dev/.claude/auth.json
  printf "%s\n" legacy-history > "$1"
  python3 -c '\''import json
p = "/home/dev/.claude/settings.json"
value = json.load(open(p))
value["fixtureUnmanaged"] = {"marker": "legacy-upgrade"}
open(p, "w").write(json.dumps(value, indent=2) + "\n")'\''
' subyard "$HISTORY_PATH"
# A yard updated to v0.14 may retain the original v0.3-v0.5 dispatcher.
guest sh -eu -c 'printf "%s\n" "$1" > /usr/local/libexec/subyard/projects-changed' \
  subyard "$(cat "$ROOT/internal/adapters/reconcileruntime/testdata/projects-changed-v0.3.sh")"
[ "$(guest sha256sum /usr/local/libexec/subyard/projects-changed | awk '{print $1}')" = \
  b632dd04a13ba11abff0b7502785ae09d8a6119c0c1261f338861796de82df35 ] \
  || die 'historical dispatcher fixture changed'
BASELINE_FINGERPRINT="$(legacy_fingerprint)"
HOST_CONFIG_FINGERPRINT="$(operator_env sha256sum "$HOST_CONFIG" | awk '{print $1}')"

info 'declining the candidate-owned update plan before any integration adoption'
run_declined "$STATE_ROOT/update-declined.log" "file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION"
assert_adoption_plan "$STATE_ROOT/update-declined.log"
assert_unadopted "$BASELINE_FINGERPRINT"
find_candidate_launcher \
  || die 'could not identify the one verified published candidate launcher'

info 'refusing candidate init without installed-owner legacy continuation'
set +e
operator_env "$CANDIDATE_LAUNCHER" init >"$STATE_ROOT/init-continuation-required.log" 2>&1
init_rc=$?
set -e
[ "$init_rc" -eq 1 ] \
  && grep -Fq 'existing unmarked project has no continuation consent; use yard init --adopt to assess adoption for normal management, then rerun yard init to reconcile; alternatively, use yard init --continue-legacy to assess limited normal reconciliation without deletion ownership' \
    "$STATE_ROOT/init-continuation-required.log" \
  || die 'candidate init did not refuse the exact missing-continuation boundary'
assert_unadopted "$BASELINE_FINGERPRINT"

info 'rejecting a changed legacy plain file without claiming or replacing it'
guest sh -eu -c 'cp "$1" /tmp/subyard-legacy-rules; printf "%s\n" changed-legacy-rule >> "$1"' \
  subyard "$PLAIN_PATH"
PLAIN_DRIFT_FINGERPRINT="$(legacy_fingerprint)"
set +e
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes \
  >"$STATE_ROOT/plain-drift.log" 2>&1
plain_rc=$?
set -e
[ "$plain_rc" -ne 0 ] || die 'published updater adopted a changed legacy plain file'
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
info 'reporting unknown dispatcher contents without changing the file or release'
guest sh -eu -c 'printf "\n# unknown local change\n" >> /usr/local/libexec/subyard/projects-changed'
DISPATCHER_DRIFT_FINGERPRINT="$(legacy_fingerprint)"
if ! operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --check \
  >"$STATE_ROOT/dispatcher-check.json" 2>"$STATE_ROOT/dispatcher-check.log"; then
  die 'dispatcher check did not produce a readable inspection'
fi
jq -e '
  .outcome.reachedGoal == false and
  (.outcome.message | contains("contents do not match")) and
  (.outcome.message | contains("/usr/local/libexec/subyard/projects-changed")) and
  (.outcome.retry | contains("sha256sum")) and
  (.blockers[0].message == .outcome.message)
' "$STATE_ROOT/dispatcher-check.json" >/dev/null || die 'check hid the actionable dispatcher conflict'
assert_unadopted "$DISPATCHER_DRIFT_FINGERPRINT"
guest sh -eu -c 'printf "%s\n" "$1" > /usr/local/libexec/subyard/projects-changed' \
  subyard "$(cat "$ROOT/internal/adapters/reconcileruntime/testdata/projects-changed-v0.3.sh")"
assert_unadopted "$BASELINE_FINGERPRINT"
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
assert_paseo_preserved() {
  assert_unadopted "$BASELINE_FINGERPRINT"
  [ "$(guest sha256sum /etc/systemd/system/paseo.service /srv/agents/paseo/data/cleanup-sentinel)" = "$paseo_before" ] \
    || die 'read-only inspection changed preserved Paseo artifacts'
  guest sh -eu -c '
    systemctl is-active --quiet paseo.service
    systemctl is-enabled --quiet paseo.service
    [ ! -e /etc/systemd/system/paseo.service.subyard-retired ]
    [ ! -e /var/lib/subyard/paseo-ownership ]
  '
}
assert_paseo_preserved
# The frozen release protocol carries one bounded message and one safe retry.
# Require its read-only cleanup retry, then use candidate-owned public status for
# the full cleanup preview/confirmation hint; neither read may change ownership.
if ! operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --check \
  >"$STATE_ROOT/paseo-check.json" 2>"$STATE_ROOT/paseo-check.log"; then
  die 'Paseo check did not produce a readable inspection'
fi
paseo_conflict_message='integration paseo is not selected in CODING_TOOL_INTEGRATIONS, but /etc/systemd/system/paseo.service remains and ownership cannot be verified using /var/lib/subyard/paseo-ownership; service preserved (releases before 0.15 did not create this receipt)'
jq -e --arg message "$paseo_conflict_message" '.outcome.reachedGoal == false and .outcome.message == $message and (.outcome.message == .blockers[0].message) and (.outcome.retry == .blockers[0].retry)' \
  "$STATE_ROOT/paseo-check.json" >/dev/null || die 'Paseo check omitted its public ownership conflict'
python3 - "$STATE_ROOT/paseo-check.json" "${CANDIDATE_LAUNCHER%/yard}/yard-engine" <<'PYCHECK'
import json
import shlex
import sys
inspection = json.load(open(sys.argv[1]))
expected = [sys.argv[2], "-Y", "default", "integration", "cleanup", "paseo", "--check"]
assert shlex.split(inspection["outcome"]["retry"]) == expected, inspection["outcome"]
PYCHECK
assert_paseo_preserved
operator_env "${CANDIDATE_LAUNCHER%/yard}/yard-engine" -Y default integration status paseo --json \
  >"$STATE_ROOT/paseo-status.json" || die 'candidate Paseo status could not be observed'
jq -e '.yard == "default" and .id == "paseo" and .observed == "conflict" and .observed_scope == "yard" and (.selection.effective | index("paseo") | not)' \
  "$STATE_ROOT/paseo-status.json" >/dev/null || die 'candidate Paseo status reported the wrong scope or state'
assert_paseo_preserved
# Parse public commands as argv and validate the exact candidate and bounded action.
# Never evaluate diagnostic text as shell code or derive unchecked apply argv.
python3 - "$STATE_ROOT/paseo-status.json" "${CANDIDATE_LAUNCHER%/yard}/yard-engine" "$STATE_ROOT/paseo-check.json" \
  >"$STATE_ROOT/cleanup-command" <<'PYHINT'
import json
import shlex
import sys
status = json.load(open(sys.argv[1]))
inspection = json.load(open(sys.argv[3]))
lines = status["detail"].splitlines()
expected = [sys.argv[2], "-Y", "default", "integration", "cleanup", "paseo"]
message = inspection["outcome"]["message"]
assert status["detail"].startswith(message), status["detail"]
assert shlex.split(inspection["outcome"]["retry"]) == expected + ["--check"], inspection["outcome"]
commands = [shlex.split(line) for line in lines if "'integration' 'cleanup' 'paseo'" in line]
try:
    assert expected in commands and expected + ["--check"] in commands, commands
except AssertionError:
    sys.stderr.write("\n".join(lines) + "\n")
    raise
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
assert_paseo_retired() {
  [ "$(guest sha256sum /etc/systemd/system/paseo.service.subyard-retired /srv/agents/paseo/data/cleanup-sentinel)" = "$paseo_retired" ] \
    || die 'candidate cleanup retry changed preserved artifacts'
  guest sh -eu -c '
    [ ! -e /etc/systemd/system/paseo.service ]
    [ ! -L /etc/systemd/system/paseo.service ]
    [ -f /etc/systemd/system/paseo.service.subyard-retired ]
    [ ! -L /etc/systemd/system/paseo.service.subyard-retired ]
    [ ! -e /var/lib/subyard/paseo-ownership ]
    [ ! -L /var/lib/subyard/paseo-ownership ]
    [ "$(cat /srv/agents/paseo/data/cleanup-sentinel)" = legacy-paseo-data ]
    ! systemctl is-active --quiet paseo.service
    ! systemctl is-enabled --quiet paseo.service
  '
  assert_unadopted "$BASELINE_FINGERPRINT"
}
operator_env "${cleanup_command[@]}" --yes >"$STATE_ROOT/paseo-cleanup-retry.log"
# A mutation retry reports its converged typed step; --check owns the prose preview.
grep -Fxq '  [skip] integration.cleanup: subyard/yard:integration paseo' "$STATE_ROOT/paseo-cleanup-retry.log" \
  || die 'candidate cleanup retry did not skip its exact target'
if grep -Fq '[apply] integration.cleanup:' "$STATE_ROOT/paseo-cleanup-retry.log"; then
  die 'candidate cleanup retry still planned an apply'
fi
assert_paseo_retired
operator_env "${cleanup_command[@]}" --check >"$STATE_ROOT/paseo-cleanup-retry-check.log"
grep -Fq 'No cleanup needed.' "$STATE_ROOT/paseo-cleanup-retry-check.log" \
  || die 'candidate cleanup retry was not a no-op'
assert_paseo_retired

# The baseline really contains the managed predecessor. Keep a persistent history
# sentinel separate from credentials and assert compatible retirement after update.
observer_history=/srv/agents/ai-observer/data/.retirement-history
guest sh -eu -c 'test -d /srv/agents/ai-observer/data; printf "%s\n" "$2" > "$1"' subyard "$observer_history" "$MARKER"
info 'upgrading through the published updater and adopting the exact legacy state'
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
[ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] \
  || die 'candidate release was not activated'
CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
case "$CANDIDATE_RELEASE_TARGET" in
  releases/"$CANDIDATE_VERSION"-*) ;;
  *) die 'activated runtime target does not identify the candidate' ;;
esac
[[ "${CANDIDATE_RELEASE_TARGET#releases/"$CANDIDATE_VERSION"-}" =~ ^[0-9a-f]{12}$ ]] \
  || die 'activated runtime target has an invalid artifact identity'
guest test -f "$INVENTORY" || die 'successful upgrade did not publish integration inventory'
guest sh -eu -c '
  [ "$(cat "$1")" = "$2" ]
  [ ! -e /usr/local/bin/ai-observer ]
  [ ! -e /etc/systemd/system/subyard-ai-observer.service ]
  ! docker container ls -a --format "{{.Names}}" | grep -Fxq subyard-ai-observer
  [ ! -e /var/lib/subyard/openlit ]
' subyard "$observer_history" "$MARKER" || die 'retirement lost history or retained the predecessor controls'
for key in user.subyard.ai_observer_provision user.subyard.ai_observer_proxy; do
  [ -z "$(incus config get "$INSTANCE" "$key" --project "$PROJECT")" ] || die 'retirement retained Observer receipt'
done
! incus config device list "$INSTANCE" --project "$PROJECT" | grep -Fxq ai-observer || die 'retirement retained Observer route'

[ "$(guest sha256sum /usr/local/libexec/subyard/projects-changed | awk '{print $1}')" = \
  "$(sha256sum "$ROOT/config/projects-changed.sh" | awk '{print $1}')" ] \
  || die 'successful upgrade did not replace the historical dispatcher'
operator_yard integration status --json > "$STATE_ROOT/adopted-status.json"
jq -e '
  .observed == "ready" and
  .selection.requested == ["claude","codex","opencode","pi"]
' "$STATE_ROOT/adopted-status.json" >/dev/null \
  || die 'successful upgrade did not preserve the four current integrations'
assert_preserved_user_state

# Prove the installed candidate carries the declaration and uses both resolved
# sources through release activation. The baseline already copied these raw bytes.
find_candidate_launcher || die 'activated candidate package is unavailable'
candidate_root="${CANDIDATE_LAUNCHER%/bin/yard}"
operator_env jq -e '.schema_version == 2 and (.artifacts | length) == 2' \
  "$candidate_root/config/agents/opencode/integration.json" >/dev/null \
  || die 'activated runtime lacks the artifact declaration'
[ "$(guest sha256sum /home/dev/.config/opencode/opencode.jsonc | awk '{print $1}')" = \
  "$(operator_env sha256sum "$OPENCODE_CONFIG" | awk '{print $1}')" ] \
  || die 'artifact activation changed raw JSONC bytes'
guest sh -eu -c '
  grep -Fq "OpenCode operator instructions survive artifact cutover." /home/dev/.config/opencode/AGENTS.md
  ! grep -Fq "<!-- subyard-preview -->" /home/dev/.config/opencode/AGENTS.md
  [ "$(stat -c "%a:%u" /home/dev/.config/opencode/AGENTS.md)" = "644:2000" ]
  [ "$(stat -c "%a:%u" /home/dev/.config/opencode/opencode.jsonc)" = "644:2000" ]
' || die 'declared instructions lost content or permissions, or retained legacy preview text'
guest sh -eu -c '
  skill=/usr/local/share/subyard/skills/subyard-preview
  [ "$(stat -c "%a:%u:%g" "$skill/SKILL.md")" = "644:0:0" ]
  [ "$(readlink /home/dev/.config/opencode/skills/subyard-preview)" = "$skill" ]
  [ "$(stat -c "%u:%g" /home/dev/.config/opencode/skills/subyard-preview)" = "2000:2000" ]
' || die 'preview skill delivery or OpenCode discovery is invalid'
[ "$(guest sha256sum /usr/local/share/subyard/skills/subyard-preview/SKILL.md | awk '{print $1}')" = \
  "$(operator_env sha256sum "$candidate_root/config/skills/core/subyard-preview/SKILL.md" | awk '{print $1}')" ] \
  || die 'preview skill differs from the activated runtime'

info 'preparing observed native capabilities through the owning host-memory backend'
assert_project_owned
[ -c /dev/kvm ] || die 'native capability fixture requires the allocated VM KVM device'
operator_yard config set ENVIRONMENT_PROFILES 'lifecycle-native lifecycle-other' --scope host --local --yes
# KVM is a synthetic pre-existing device; host-memory creation must use its
# production owner, including the actual read-only proc mount verification.
incus config device add "$INSTANCE" yx-dev-kvm unix-char --project "$PROJECT" \
  source=/dev/kvm path=/dev/kvm mode=0666
driver="$ROOT/.build/legacy-native-owner"
install -d -m 0700 "$driver"
cat > "$driver/main.go" <<'NATIVE_OWNER'
package main
import (
 "context"
 "os"
 "path/filepath"
 "github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
 "github.com/Subyard/Subyard/internal/config"
)
func main() {
 if len(os.Args)!=4 {panic("native fixture requires runtime, operator home and marker")}
 root,home,marker:=os.Args[1],os.Args[2],os.Args[3]
 ctx:=context.Background()
 runner:=testvmsruntime.ProcessRunner{}
 actual,_,err:=runner.Run(ctx,"incus",[]string{"project","get","subyard","user.subyard.p0-power-systemd"},nil,nil)
 if err!=nil || string(actual)!=marker+"\n" {panic("native fixture project marker differs")}
 loaded,err:=config.Load(config.LoadOptions{RepositoryRoot:root,OperatorHome:home,YardName:"default",Environment:map[string]string{"HOME":home,"SUBYARD_HOME":filepath.Join(home,".subyard"),"SUBYARD_CONFIG_HOME":filepath.Join(home,".config/subyard")}})
 if err!=nil {panic(err)}
 backend:=testvmsruntime.Backend{RepositoryRoot:root,DataHome:loaded.Context.Paths.DataHome,Dispatcher:filepath.Join(root,"bin/yard-engine"),Project:loaded.Context.IncusProject,Instance:loaded.Context.YardInstanceName,YardName:"default",DesiredPower:"running",HostMemory:true,Environment:loaded.Environment,Output:os.Stderr}
 if err:=backend.Apply(ctx);err!=nil {panic(err)}
 if ready,err:=backend.Verify(ctx);err!=nil || !ready {panic("native host-memory backend verification failed")}
}
NATIVE_OWNER
go build -o "$driver/owner" "$driver/main.go"
sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0755 "$driver/owner" "$OPERATOR_HOME/native-profile-owner"
operator_env "$OPERATOR_HOME/native-profile-owner" "$candidate_root" "$OPERATOR_HOME" "$MARKER"
sha256sum "$driver/main.go" "$driver/owner"

continuation_grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
operator_env test ! -e "$continuation_grant" && operator_env test ! -L "$continuation_grant" \
  || die 'legacy continuation was established before explicit consent'
inventory_before="$(guest sha256sum "$INVENTORY")"
config_before="$(yard_config_fingerprint)"
state_before="$(legacy_fingerprint)"
continuation_host_before="$(operator_env sha256sum "$HOST_CONFIG")"
runtime_links_before="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current" "$OPERATOR_HOME/.subyard/runtime/previous")"
instructions_before="$(guest sha256sum /home/dev/.config/opencode/AGENTS.md)"
artifact_modes_before="$(guest stat -c '%a:%u:%g' /home/dev/.config/opencode/AGENTS.md /home/dev/.config/opencode/opencode.jsonc)"
info 'verifying candidate-owned installation enrollment after authentic updater adoption'
operator_env test -f "$OPERATOR_HOME/.subyard/runtime/installation.json" \
  || die 'successful historical update omitted runtime installation inventory'
[ "$(operator_env stat -c '%a:%u' "$OPERATOR_HOME/.subyard/runtime/installation.json")" = "600:$(operator_env id -u)" ] \
  || die 'candidate-owned runtime inventory has unsafe permissions'
[ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current" "$OPERATOR_HOME/.subyard/runtime/previous")" = "$runtime_links_before" ] \
  && [ "$(guest sha256sum /home/dev/.config/opencode/AGENTS.md)" = "$instructions_before" ] \
  && [ "$(guest stat -c '%a:%u:%g' /home/dev/.config/opencode/AGENTS.md /home/dev/.config/opencode/opencode.jsonc)" = "$artifact_modes_before" ] \
  && [ "$(guest sha256sum "$INVENTORY")" = "$inventory_before" ] \
  && [ "$(yard_config_fingerprint)" = "$config_before" ] \
  && [ "$(legacy_fingerprint)" = "$state_before" ] \
  && [ "$(operator_env sha256sum "$HOST_CONFIG")" = "$continuation_host_before" ] \
  || die 'candidate-owned enrollment changed activation or adopted state'
operator_env test ! -e "$continuation_grant" && operator_env test ! -L "$continuation_grant" \
  || die 'candidate-owned enrollment granted legacy continuation'
[ -z "$(incus project get "$PROJECT" user.subyard.installation)" ] \
  || die 'candidate-owned enrollment acquired destructive installation ownership'

info 'declining installed-owner continuation without changing the adopted state'
run_declined "$STATE_ROOT/continuation-declined.log" "file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" init --continue-legacy
grep -Fq '[apply] init.legacy-continuation:' "$STATE_ROOT/continuation-declined.log" \
  || die 'continuation decline did not assess its explicit typed grant'
operator_env test ! -e "$continuation_grant" && operator_env test ! -L "$continuation_grant" \
  || die 'declined continuation wrote a protected grant'
[ "$(guest sha256sum "$INVENTORY")" = "$inventory_before" ] \
  && [ "$(yard_config_fingerprint)" = "$config_before" ] \
  && [ "$(legacy_fingerprint)" = "$state_before" ] \
  && [ "$(operator_env sha256sum "$HOST_CONFIG")" = "$continuation_host_before" ] \
  || die 'declined continuation changed adopted artifacts or settings'
[ -z "$(incus project get "$PROJECT" user.subyard.installation)" ] \
  || die 'declined continuation acquired destructive installation ownership'

info 'establishing limited continuation through the active installed owner'
# This shared helper also owns any required disk-/tmp restart and the exact
# activation-only repair, while proving no full removal ownership is acquired.
establish_legacy_continuation

info 'provisioning only the requested selected hook after exact native continuation'
run_declined "$STATE_ROOT/named-provision-declined.log" "file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" provision lifecycle-native
guest test ! -e /srv/subyard-lifecycle-native-provisioned
guest test ! -e /srv/subyard-lifecycle-other-provisioned
operator_yard provision lifecycle-native --yes
guest test -f /srv/subyard-lifecycle-native-provisioned
guest test ! -e /srv/subyard-lifecycle-other-provisioned
hook_before="$(guest stat -c '%d:%i:%Y:%Z' /srv/subyard-lifecycle-native-provisioned)"
operator_yard provision lifecycle-native --yes
[ "$(guest stat -c '%d:%i:%Y:%Z' /srv/subyard-lifecycle-native-provisioned)" = "$hook_before" ] \
  || die 'matching named provision replayed its hook'
guest test ! -e /srv/subyard-lifecycle-other-provisioned

info 'repeating init without materializing an inherited selection'
config_before="$(yard_config_fingerprint)"
operator_yard init --yes
[ "$(yard_config_fingerprint)" = "$config_before" ] \
  || die 'init rewrote the inherited default-yard settings'
operator_env test ! -e "$YARD_CONFIG" \
  || die 'init materialized a default-yard selection from inherited settings'
operator_yard integration status --json | jq -e '
  .observed == "ready" and
  .selection.requested == ["claude","codex","opencode","pi"]
' >/dev/null || die 'explicit init changed the inherited requested set'
assert_preserved_user_state

info 'repeating init without rewriting stable integration evidence'
inventory_before="$(guest sha256sum "$INVENTORY")"
config_before="$(yard_config_fingerprint)"
state_before="$(legacy_fingerprint)"
operator_yard init --yes
[ "$(guest sha256sum "$INVENTORY")" = "$inventory_before" ] \
  || die 'repeated init rewrote stable integration inventory'
[ "$(yard_config_fingerprint)" = "$config_before" ] \
  || die 'repeated init rewrote inherited default-yard settings'
[ "$(legacy_fingerprint)" = "$state_before" ] \
  || die 'repeated init changed stable integration artifacts'

info 'disabling and re-enabling Claude while preserving user-owned state'
operator_yard integration disable claude --yes
operator_yard integration status --json > "$STATE_ROOT/disabled-status.json"
jq -e '
  .observed == "ready" and .selection.requested == ["codex","opencode","pi"]
' "$STATE_ROOT/disabled-status.json" >/dev/null \
  || {
    jq -c '{observed, requested: .selection.requested}' "$STATE_ROOT/disabled-status.json" >&2 || true
    die 'Claude disable did not preserve the three remaining integrations'
  }
guest test ! -L "$LINK_PATH" || die 'Claude disable retained the owned history link'
assert_preserved_user_state

operator_yard integration enable claude --yes
operator_yard integration status --json > "$STATE_ROOT/re-enabled-status.json"
# Enabling appends to the persisted requested order; it does not restore defaults.
jq -e '
  .observed == "ready" and .selection.requested == ["codex","opencode","pi","claude"]
' "$STATE_ROOT/re-enabled-status.json" >/dev/null \
  || {
    jq -c '{observed, requested: .selection.requested}' "$STATE_ROOT/re-enabled-status.json" >&2 || true
    die 'Claude re-enable did not restore the four current integrations'
  }
[ "$(guest readlink "$LINK_PATH")" = "$LINK_TARGET" ] \
  || die 'Claude re-enable did not restore the owned history link'
assert_preserved_user_state

info 'refusing incompatible v0.14.0 rollback without changing adopted state'
config_before="$(yard_config_fingerprint)"
inventory_before="$(guest sha256sum "$INVENTORY")"
state_before="$(legacy_fingerprint)"
verify_legacy_rollback_refusal
[ "$(yard_config_fingerprint)" = "$config_before" ] \
  && [ "$(guest sha256sum "$INVENTORY")" = "$inventory_before" ] \
  && [ "$(legacy_fingerprint)" = "$state_before" ] \
  || die 'refused rollback changed integration artifacts or settings'
assert_preserved_user_state
assert_legacy_continuation

info 'retrying the active candidate after incompatible rollback refusal'
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
operator_yard integration status --json > "$STATE_ROOT/re-upgraded-status.json"
jq -e '.observed == "ready" and .selection.requested == ["codex","opencode","pi","claude"]' \
  "$STATE_ROOT/re-upgraded-status.json" >/dev/null \
  || {
    jq -c '{observed, requested: .selection.requested}' "$STATE_ROOT/re-upgraded-status.json" >&2 || true
    die 'candidate did not remain converged after refused rollback and update retry'
  }
assert_preserved_user_state
assert_legacy_continuation

ok 'pinned v0.14.0 adoption, refusal, decline, lifecycle preservation, rollback refusal and candidate retry passed'
