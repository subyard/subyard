#!/usr/bin/env bash
# Host-free behavior contract for the thin official Hermes bootstrap.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE_DIR="$ROOT/config/profiles/hermes"
HOOK="$PROFILE_DIR/provision.sh"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -x "$HOOK" ] || fail 'Hermes provision hook is not executable'
mkdir -p "$TMP/bin" "$TMP/home" "$TMP/root"
LOG="$TMP/commands.log"
INSTALLER_LOG="$TMP/installer.log"
export HERMES_TEST_LOG="$LOG" HERMES_TEST_INSTALLER_LOG="$INSTALLER_LOG"

cat >"$TMP/release.json" <<'JSON'
{"tag_name":"v2026.8.13","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.13","draft":false,"prerelease":false,"published_at":"2026-08-13T20:37:37Z"}
JSON
cat >"$TMP/official-install.sh" <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
[ "$PWD" = "$HERMES_TEST_EXPECTED_CWD" ]
printf 'user=%s\nhome=%s\nhermes_home=%s\n' \
  "${HERMES_EFFECTIVE_USER:-}" "$HOME" "${HERMES_HOME:-}" >>"$HERMES_TEST_INSTALLER_LOG"
printf 'node_deps_timeout=%s\n' "${NODE_DEPS_TIMEOUT:-}" >>"$HERMES_TEST_INSTALLER_LOG"
printf 'argv=' >>"$HERMES_TEST_INSTALLER_LOG"
printf ' <%s>' "$@" >>"$HERMES_TEST_INSTALLER_LOG"
printf '\n' >>"$HERMES_TEST_INSTALLER_LOG"

state="$HOME/.hermes"
source="$state/hermes-agent"
mkdir -p "$source/.git" "$source/venv/bin" "$HOME/.local/bin" "$state/node/bin" \
  "$state/cron" "$state/sessions" "$state/logs" "$state/memories" "$state/skills"
printf 'git\n' >"$source/.install_method"
cat >"$source/venv/bin/python" <<'PYTHON'
#!/usr/bin/env bash
exec python3 "$@"
PYTHON
chmod 0755 "$source/venv/bin/python"
printf '#!/usr/bin/env python3\n' >"$source/hermes"
cat >"$HOME/.local/bin/hermes" <<HERMES
#!/usr/bin/env bash
unset PYTHONPATH
unset PYTHONHOME
exec "$source/venv/bin/python" "$source/hermes" "\$@"
HERMES
chmod 0755 "$HOME/.local/bin/hermes"
cat >"$state/node/bin/npx" <<'UNTRUSTED'
#!/usr/bin/env bash
printf 'dev-owned-npx-executed\n' >>"$HERMES_TEST_LOG"
exit 97
UNTRUSTED
chmod 0755 "$state/node/bin/npx"
# These files are deliberately created by the upstream fixture. Subyard must preserve them opaquely.
printf 'UPSTREAM_PLACEHOLDER=\n' >"$state/.env"
chmod 0600 "$state/.env"
printf 'config_version: 1\n' >"$state/config.yaml"
printf 'upstream-owned\n' >"$state/skills/seeded-by-installer"
INSTALLER
chmod 0755 "$TMP/official-install.sh"

cat >"$TMP/bin/apt-get" <<'APT'
#!/usr/bin/env bash
printf 'apt-get' >>"$HERMES_TEST_LOG"
printf ' <%s>' "$@" >>"$HERMES_TEST_LOG"
printf '\n' >>"$HERMES_TEST_LOG"
APT

cat >"$TMP/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output) output="$2"; shift 2 ;;
    http://*|https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
[ -n "$output" ] && [ -n "$url" ]
printf 'curl <%s>\n' "$url" >>"$HERMES_TEST_LOG"
cp "$HERMES_TEST_INSTALLER_FIXTURE" "$output"
CURL

cat >"$TMP/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
printf 'git' >>"$HERMES_TEST_LOG"
printf ' <%s>' "$@" >>"$HERMES_TEST_LOG"
printf '\n' >>"$HERMES_TEST_LOG"
if [ "${1:-}" = -C ] && [ "${3:-}" = remote ] && [ "${4:-}" = get-url ]; then
  printf '%s\n' "${HERMES_TEST_GIT_ORIGIN:-https://github.com/NousResearch/hermes-agent.git}"
  exit 0
fi
if [ "${1:-}" = ls-remote ]; then
  if [ "${HERMES_TEST_GIT_HANG:-0}" = 1 ]; then
    trap '' TERM
    while :; do sleep 1; done
  fi
  printf '%s\trefs/tags/v2026.8.13\n' 0123456789abcdef0123456789abcdef01234567
  printf '%s\trefs/tags/v2026.8.13^{}\n' 89abcdef0123456789abcdef0123456789abcdef
  exit 0
fi
if [ "${1:-}" = clone ]; then
  destination="${!#}"
  mkdir -p "$destination/scripts"
  cp "$HERMES_TEST_INSTALLER_FIXTURE" "$destination/scripts/install.sh"
  chmod 0755 "$destination/scripts/install.sh"
  exit 0
fi
if [ "${1:-}" = -C ] && [ "${3:-}" = rev-parse ] && [ "${4:-}" = HEAD ]; then
  printf '%s\n' 89abcdef0123456789abcdef0123456789abcdef
  exit 0
fi
exit 91
GIT

cat >"$TMP/bin/runuser" <<'RUNUSER'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = -u ]
user="$2"
shift 2
[ "${1:-}" = -- ]
shift
printf 'runuser <%s>' "$user" >>"$HERMES_TEST_LOG"
printf ' <%s>' "$@" >>"$HERMES_TEST_LOG"
printf '\n' >>"$HERMES_TEST_LOG"
exec env HERMES_EFFECTIVE_USER="$user" "$@"
RUNUSER

cat >"$TMP/bin/install" <<'INSTALL'
#!/usr/bin/env bash
set -euo pipefail
if [ "${HERMES_TEST_REQUIRE_DEV_STATE_MUTATION:-0}" = 1 ] \
  && [ "${!#}" = "${HERMES_TEST_STATE_ROOT:?}" ] \
  && [ -z "${HERMES_EFFECTIVE_USER:-}" ]; then
  printf 'state-root install escaped the dev privilege boundary\n' >&2
  exit 96
fi
exec /usr/bin/install "$@"
INSTALL

cat >"$TMP/bin/chmod" <<'CHMOD'
#!/usr/bin/env bash
set -euo pipefail
for argument in "$@"; do
  if [ "${HERMES_TEST_REQUIRE_DEV_STATE_MUTATION:-0}" = 1 ] \
    && [ "$argument" = "${HERMES_TEST_STATE_ROOT:?}" ] \
    && [ -z "${HERMES_EFFECTIVE_USER:-}" ]; then
    printf 'state-root chmod escaped the dev privilege boundary\n' >&2
    exit 97
  fi
  case "$argument" in
    /var/tmp/subyard-hermes-bootstrap.*)
      if [ "${HERMES_TEST_REQUIRE_DEV_STATE_MUTATION:-0}" = 1 ] \
        && [ -e "${HERMES_TEST_BOOTSTRAP_CHOWNED:?}" ] \
        && [ -z "${HERMES_EFFECTIVE_USER:-}" ]; then
        printf 'bootstrap chmod ran as root after dev ownership transfer\n' >&2
        exit 98
      fi
      ;;
  esac
done
exec /usr/bin/chmod "$@"
CHMOD

cat >"$TMP/bin/chown" <<'CHOWN'
#!/usr/bin/env bash
set -euo pipefail
case "${!#}" in
  /var/tmp/subyard-hermes-bootstrap.*)
    : >"${HERMES_TEST_BOOTSTRAP_CHOWNED:?}"
    ;;
esac
exec /usr/bin/chown "$@"
CHOWN

cat >"$TMP/bin/bash" <<'BASH'
#!/bin/bash
set -euo pipefail
if [ "${1:-}" = -n ]; then
  case "${2:-}" in
    /var/tmp/subyard-hermes-bootstrap.*)
      if [ "${HERMES_TEST_REQUIRE_DEV_STATE_MUTATION:-0}" = 1 ] \
        && [ -z "${HERMES_EFFECTIVE_USER:-}" ]; then
        printf 'bootstrap installer validation escaped the dev privilege boundary\n' >&2
        exit 99
      fi
      ;;
  esac
fi
exec /usr/bin/bash "$@"
BASH
chmod 0755 "$TMP/bin/"*

dev_user="$(id -un)"
dev_group="$(id -gn)"
common_env=(
  PATH="$TMP/bin:$PATH"
  DEV_USER="$dev_user"
  HERMES_DEV_HOME="$TMP/home"
  HERMES_TEST_ROOT="$TMP"
  HERMES_RELEASE_API_FILE="$TMP/release.json"
  HERMES_TEST_INSTALLER_FIXTURE="$TMP/official-install.sh"
  HERMES_TEST_EXPECTED_CWD="$TMP/home"
  HERMES_TEST_STATE_ROOT="$TMP/home/.hermes"
  HERMES_TEST_BOOTSTRAP_CHOWNED="$TMP/bootstrap-chowned"
  HERMES_TEST_ALLOW_NON_ROOT=1
)

state="$TMP/home/.hermes"
launcher="$TMP/home/.local/bin/hermes"
mkdir -p "$state"
chmod 0700 "$state"
printf 'operator-state-before-timeout\n' >"$state/operator-owned.opaque"
chmod 0600 "$state/operator-owned.opaque"
state_signature() {
  {
    find "$state" -xdev -printf '%P %y %m %U:%G\n'
    find "$state" -xdev -type f -exec sha256sum {} \;
    if [ -e "$launcher" ] || [ -L "$launcher" ]; then
      find "$launcher" -maxdepth 0 -printf '%p %y %m %U:%G %l\n'
      [ ! -f "$launcher" ] || sha256sum "$launcher"
    else
      printf '%s missing\n' "$launcher"
    fi
  } | sort | sha256sum
}
effect_count() {
  local pattern="$1" file="$2"
  grep -Ec -- "$pattern" "$file" 2>/dev/null || true
}
check_effect_signature() {
  printf '%s %s %s %s\n' \
    "$(effect_count '^apt-get' "$LOG")" \
    "$(effect_count '^git <ls-remote>' "$LOG")" \
    "$(effect_count '^git <clone>' "$LOG")" \
    "$(effect_count '^argv=' "$INSTALLER_LOG")"
}
timeout_state_before="$(state_signature)"
set +e
env "${common_env[@]}" \
  HERMES_TEST_GIT_HANG=1 \
  HERMES_TEST_RELEASE_LOOKUP_TIMEOUT=0.1s \
  HERMES_TEST_RELEASE_LOOKUP_KILL_AFTER=0.1s \
  bash "$HOOK" >/dev/null 2>"$TMP/lookup-timeout.err"
status=$?
set -e
[ "$status" -ne 0 ] || fail 'release-tag lookup timeout did not fail provision'
[ "$(state_signature)" = "$timeout_state_before" ] \
  || fail 'release-tag lookup timeout changed opaque Hermes state'
grep -Fq 'official release tag lookup failed or exceeded 0.1s' "$TMP/lookup-timeout.err" \
  || fail 'release-tag lookup timeout did not emit a contextual diagnostic'
if grep -Fq 'git <clone>' "$LOG"; then
  fail 'release-tag lookup timeout continued to clone the release'
fi
[ ! -e "$INSTALLER_LOG" ] || fail 'release-tag lookup timeout ran the official installer'

apt_effects_before_install="$(effect_count '^apt-get' "$LOG")"
if ! (cd "$TMP/root" && env "${common_env[@]}" \
  HERMES_TEST_REQUIRE_DEV_STATE_MUTATION=1 bash "$HOOK" >/dev/null); then
  fail 'profile hook mutated the dev-owned state root outside the dev privilege boundary'
fi

source="$state/hermes-agent"
[ -d "$source/.git" ] || fail 'official installer did not produce canonical source checkout'
[ -x "$source/venv/bin/python" ] || fail 'official installer did not produce source-local venv interpreter'
[ -f "$source/hermes" ] || fail 'official installer did not produce the checked-in entrypoint'
[ -x "$launcher" ] || fail 'official installer did not produce the canonical launcher'
[ "$(stat -c %a "$state")" = 700 ] || fail 'Hermes state root is not private mode 0700'
[ "$(stat -c %U:%G "$state")" = "$dev_user:$dev_group" ] \
  || fail 'Hermes state root has the wrong owner'

grep -Fq 'git <clone> <--quiet> <--depth> <1> <--branch> <v2026.8.13> <https://github.com/NousResearch/hermes-agent.git>' "$LOG" \
  || fail 'bootstrap script was not read from an official release-tag checkout'
if grep -Fq 'raw.githubusercontent.com/NousResearch/hermes-agent' "$LOG"; then
  fail 'bootstrap depended on the rate-limited raw file endpoint'
fi
grep -Fq "runuser <$dev_user>" "$LOG" || fail 'official installer was not launched as the yard user'
for bootstrap_access in \
  '<test> <-f> </var/tmp/subyard-hermes-bootstrap\.[^/]+/source/scripts/install\.sh>' \
  '<test> <!> <-L> </var/tmp/subyard-hermes-bootstrap\.[^/]+/source/scripts/install\.sh>' \
  '<bash> <-n> </var/tmp/subyard-hermes-bootstrap\.[^/]+/source/scripts/install\.sh>' \
  '<rm> <-rf> <--> </var/tmp/subyard-hermes-bootstrap\.[^/]+>'; do
  grep -Eq "^runuser <${dev_user}> .* ${bootstrap_access}$" "$LOG" \
    || fail "bootstrap access escaped the dev privilege boundary: $bootstrap_access"
done
grep -Fxq 'apt-get <update> <-qq>' "$LOG" \
  || fail 'profile did not run the exact generic package-index refresh'
grep -Fxq 'apt-get <install> <-y> <-qq> <build-essential> <ca-certificates> <curl> <git> <libffi-dev> <python3-dev> <xz-utils>' \
  "$LOG" || fail 'profile package bootstrap escaped the generic OS prerequisite allowlist'
[ "$(effect_count '^apt-get' "$LOG")" -eq "$((apt_effects_before_install + 2))" ] \
  || fail 'profile ran an unexpected package-manager command'
if grep -Eqi 'playwright|chromium|whisper|telegram|faster-whisper|ffmpeg|ripgrep|npx|npm' \
  "$HOOK"; then
  fail 'profile hook contains a Hermes component-specific prerequisite'
fi
grep -Fxq "user=$dev_user" "$INSTALLER_LOG" || fail 'official installer observed the wrong user'
grep -Fxq "home=$TMP/home" "$INSTALLER_LOG" || fail 'official installer observed the wrong HOME'
grep -Fxq "hermes_home=$TMP/home/.hermes" "$INSTALLER_LOG" \
  || fail 'official installer observed a non-canonical HERMES_HOME'
grep -Fxq 'node_deps_timeout=1200' "$INSTALLER_LOG" \
  || fail 'official installer did not receive the bounded slow-link Node dependency budget'
grep -Fq ' <--branch> <v2026.8.13>' "$INSTALLER_LOG" \
  || fail 'official installer did not receive the latest stable release tag'
grep -Fq ' <--commit> <89abcdef0123456789abcdef0123456789abcdef>' "$INSTALLER_LOG" \
  || fail 'official installer did not receive the immutable release commit'
grep -Fq ' <--force-commit>' "$INSTALLER_LOG" \
  || fail 'official installer was not forced to converge on the selected release commit'
grep -Fq ' <--skip-setup>' "$INSTALLER_LOG" \
  || fail 'Subyard did not defer interactive Hermes setup to the operator'
grep -Fq ' <--non-interactive>' "$INSTALLER_LOG" \
  || fail 'official bootstrap was not explicitly non-interactive'
for forbidden in --skip-browser --skip-computer-use --no-skills; do
  if grep -Fq " <$forbidden>" "$INSTALLER_LOG"; then
    fail "Subyard selected Hermes components via $forbidden"
  fi
done

printf 'operator-opaque-state\n' >"$state/operator-owned.opaque"
chmod 0600 "$state/operator-owned.opaque"
before="$(state_signature)"
installer_calls_before="$(grep -c '^argv=' "$INSTALLER_LOG")"
env "${common_env[@]}" bash "$HOOK" >/dev/null
after="$(state_signature)"
[ "$before" = "$after" ] || fail 'repeat provision changed opaque Hermes state'
[ "$(grep -c '^argv=' "$INSTALLER_LOG")" = "$installer_calls_before" ] \
  || fail 'repeat provision reran the official installer'
[ ! -e "$TMP/var/lib/subyard/hermes-playwright-deps-v1" ] \
  || fail 'Subyard recorded component-specific prerequisite state'
if grep -Eq 'playwright|chromium|admin_cwd=|^npx=' "$INSTALLER_LOG"; then
  fail 'Subyard invoked a Hermes-owned component prerequisite'
fi
if grep -Fq 'dev-owned-npx-executed' "$LOG"; then
  fail 'provision executed a dev-owned Hermes component with profile-hook authority'
fi

before_check="$after"
before_check_effects="$(check_effect_signature)"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
after_check="$(state_signature)"
[ "$before_check" = "$after_check" ] || fail 'provision check mutated Hermes state'
[ "$(check_effect_signature)" = "$before_check_effects" ] \
  || fail 'healthy provision check performed an install, package or network effect'

before_ssh_check_effects="$(check_effect_signature)"
env "${common_env[@]}" HERMES_TEST_GIT_ORIGIN=git@github.com:NousResearch/hermes-agent.git \
  bash "$HOOK" --check >/dev/null \
  || fail 'health check rejected the official SSH remote form'
[ "$(state_signature)" = "$before_check" ] \
  || fail 'SSH-origin provision check mutated canonical Hermes state'
[ "$(check_effect_signature)" = "$before_ssh_check_effects" ] \
  || fail 'SSH-origin provision check performed an install, package or network effect'

rm "$source/.install_method"
before_drift_check="$(state_signature)"
before_drift_check_effects="$(check_effect_signature)"
set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
status=$?
set -e
[ "$status" -eq 10 ] || fail "drift check status=$status, want 10"
[ "$(state_signature)" = "$before_drift_check" ] \
  || fail 'drifted provision check mutated canonical Hermes state'
[ "$(check_effect_signature)" = "$before_drift_check_effects" ] \
  || fail 'drifted provision check performed an install, package or network effect'

printf 'ok: Hermes uses official per-user bootstrap and preserves opaque canonical state\n'
