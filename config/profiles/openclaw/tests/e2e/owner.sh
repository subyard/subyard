#!/usr/bin/env bash
# Real-host OpenClaw toolchain and shared-cache acceptance on an allocated disposable VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
YARD_BIN=''

die() { printf 'openclaw-e2e: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
[ "$(id -un)" = dev ] || die 'run as the allocated dev user'
for command in go incus jq sudo; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() { "$YARD_BIN" -Y "$YARD_NAME" "$@"; }

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64997\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    yard teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-openclaw.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = openclaw-profile-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-openclaw.XXXXXX)"
printf '%s\n' openclaw-profile-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="openclaw-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/overrides/host"
printf '%s\n' "$(printf '%s' openclaw-e2e-synthetic-denylist-token | sha256sum | awk '{print $1}')" \
  > "$SUBYARD_CONFIG_HOME/overrides/host/prod-fingerprints"
chmod 0600 "$SUBYARD_CONFIG_HOME/overrides/host/prod-fingerprints"

if [ -e "$ROOT/.subyard-acceptance/candidate.json" ] || [ -L "$ROOT/.subyard-acceptance/candidate.json" ]; then
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  YARD_BIN="$(release_candidate_prepare "$ROOT")" \
    || die 'packaged release candidate is invalid or unavailable'
else
  "$ROOT/dev/build-engine.sh" --force
  YARD_BIN="$ROOT/.build/yard"
fi

port=$((35000 + ($$ % 15000)))
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
YARD_KIND=container
SSH_PORT=$port
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=openclaw
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

printf 'openclaw_e2e_stage=init-first\n'
yard init --yes
printf 'openclaw_e2e_stage=init-repeat\n'
yard init --yes

instance="yard-$YARD_NAME"
project="subyard-$YARD_NAME"
provision_first="$STATE/provision-first.out"
yard provision openclaw --yes > "$provision_first"
grep -Fq 'provisioning openclaw' "$provision_first" \
  || die 'public provision did not apply the selected OpenClaw profile hook'

check_guest_state() {
  local docs_hash
  docs_hash="$(sha256sum "$ROOT/config/profiles/openclaw/openclaw-l1.md" | awk '{print $1}')"
  # shellcheck disable=SC2016
  yard shell -- sh -eu -c '
    expected="$(id -u dev):$(id -g dev)"
    [ "$(node --version)" = v24.15.0 ]
    [ "$(corepack --version)" = 0.31.0 ]
    [ "$(pnpm --version)" = 11.2.2 ]
    python3 -c "import sys; assert sys.version_info.major == 3"
    /opt/venv/bin/python -c "import sys; assert sys.prefix == \"/opt/venv\""
    for path in /srv/cache/pnpm /srv/cache/pip /srv/cache/npm; do
      [ -d "$path" ] && [ ! -L "$path" ]
      [ "$(stat -c %u:%g "$path")" = "$expected" ]
    done
    [ -x /usr/local/bin/sy-cache ]
    [ -f /srv/cache/.sy-cache.lock ] && [ ! -L /srv/cache/.sy-cache.lock ]
    [ "$(stat -c %u:%g /srv/cache/.sy-cache.lock)" = "$expected" ]
    [ -f /home/dev/.npmrc ] && grep -Fxq "cache=/srv/cache/npm" /home/dev/.npmrc
    [ "$(stat -c %u:%g /home/dev/.npmrc)" = "$expected" ]
    [ -f /home/dev/.config/pip/pip.conf ]
    grep -Fxq "cache-dir = /srv/cache/pip" /home/dev/.config/pip/pip.conf
    [ "$(stat -c %u:%g /home/dev/.config/pip/pip.conf)" = "$expected" ]
    [ -f /etc/profile.d/subyard-openclaw.sh ]
    grep -Fq "SUBYARD_OPENCLAW_DOCS=/etc/subyard/openclaw-l1.md" /etc/profile.d/subyard-openclaw.sh
    [ -f /etc/environment ] && grep -Fxq "SUBYARD_OPENCLAW_DOCS=/etc/subyard/openclaw-l1.md" /etc/environment
    [ -f /etc/subyard/openclaw-l1.md ]
    [ "$(sha256sum /etc/subyard/openclaw-l1.md | cut -d " " -f1)" = "$1" ]
  ' sh "$docs_hash"
}
check_guest_state

# Exercise staging container bring-up and teardown with a synthetic denylist fingerprint.
# Up leaves the gateway stopped; it does not need source code, bot credentials, or exclusive-key consent.
staging_status="$STATE/staging-status.out"
qa_status="$STATE/qa-status.out"
yard staging status > "$staging_status"
yard qa-pool status > "$qa_status"
grep -Fq "(no runner)" "$staging_status" || die 'fresh yard unexpectedly has a staging runner'
grep -Fq "(no broker)" "$qa_status" || die 'fresh yard unexpectedly has a QA broker'
yard staging up --yes > "$STATE/staging-up.out"
yard staging status > "$staging_status"
grep -Fq 'subyard-staging-canonical' "$staging_status" \
  || die 'staging status did not report its brought-up runner'
grep -Fq 'gateway: down' "$staging_status" \
  || die 'staging bring-up unexpectedly started the gateway'
if yard qa-pool up --yes > "$STATE/qa-up.out" 2>&1; then
  die 'QA resource accepted bring-up without a broker source and generated secrets'
fi
grep -Fq 'BROKER_SRC unset' "$STATE/qa-up.out" \
  || die 'QA resource failed for a reason other than its missing broker source'
yard staging down --yes > "$STATE/staging-down.out"
yard staging status > "$staging_status"
grep -Fq 'exited' "$staging_status" || die 'staging down did not stop the runner'
yard staging destroy --yes > "$STATE/staging-destroy.out"
yard staging status > "$staging_status"
incus exec "$instance" --project "$project" -- test -d /srv/staging/canonical \
  || die 'staging destroy removed persistent zone data without --purge'
yard qa-pool status > "$qa_status"
grep -Fq "(no runner)" "$staging_status" || die 'staging destroy left its runner behind'
grep -Fq "(no broker)" "$qa_status" || die 'refused QA bring-up left a broker behind'

# A converged public provision must perform no profile apply or cache/config rewrite.
before="$(incus exec "$instance" --project "$project" -- sh -eu -c \
  'stat -c "%u:%g:%Y:%s" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/.sy-cache.lock /home/dev/.npmrc /home/dev/.config/pip/pip.conf')"
provision_repeat="$STATE/provision-repeat.out"
yard provision openclaw --yes > "$provision_repeat"
if grep -Fq 'provisioning openclaw' "$provision_repeat"; then
  die 'converged public provision ran the OpenClaw apply hook again'
fi
after="$(incus exec "$instance" --project "$project" -- sh -eu -c \
  'stat -c "%u:%g:%Y:%s" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/.sy-cache.lock /home/dev/.npmrc /home/dev/.config/pip/pip.conf')"
[ "$before" = "$after" ] || die 'converged provision changed cache/config metadata'
check_guest_state

# This lane covers public provisioning and the staging container lifecycle. The external
# obligations in external-obligations.json remain separately incomplete.
printf 'ok: OpenClaw init, toolchain/cache ownership, provisioning idempotency, and staging container lifecycle\n'
