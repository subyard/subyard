#!/usr/bin/env bash
# Real-host provisioning acceptance for subyard-dev on the allocated owner VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
CANDIDATE_ENGINE=''

die() { printf 'subyard-dev-e2e: %s\n' "$*" >&2; exit 2; }
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
yard() {
  if [ -n "$CANDIDATE_ENGINE" ]; then
    "$CANDIDATE_ENGINE" -Y "$YARD_NAME" "$@"
  else
    "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"
  fi
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    yard teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-subyard-dev.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-dev-profile-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-subyard-dev.XXXXXX)"
printf '%s\n' subyard-dev-profile-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="dev-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"

# A release run must exercise the frozen packaged runtime. Ordinary development
# acceptance has no metadata and builds the current development engine instead.
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
set +e
CANDIDATE_ENGINE="$(release_candidate_prepare "$ROOT")"
candidate_status=$?
set -e
case "$candidate_status" in
  0) ;;
  1) "$ROOT/dev/build-engine.sh" --force ;;
  *) die 'frozen release candidate is invalid' ;;
esac
port=$((35000 + ($$ % 15000)))
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
YARD_KIND=container
SSH_PORT=$port
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=subyard-dev
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

printf 'subyard-dev_e2e_stage=init-first\n'
yard init --yes
printf 'subyard-dev_e2e_stage=init-repeat\n'
yard init --yes

instance="yard-$YARD_NAME"
project="subyard-$YARD_NAME"
provision_log="$STATE/provision-first.out"
yard provision subyard-dev --yes > "$provision_log"
grep -Fq 'provisioning subyard-dev' "$provision_log" \
  || die 'public provision did not apply the selected profile hook'

check_guest_state() {
  # shellcheck disable=SC2016
  yard shell -- sh -eu -c '
    user=dev
    group_id="$(id -g "$user")"
    expected="$(id -u "$user"):$group_id"
    for path in /srv/cache/go-build /srv/cache/go-mod /home/dev/.config/go; do
      [ -d "$path" ] && [ ! -L "$path" ] || exit 11
      [ "$(stat -c "%u:%g" "$path")" = "$expected" ] || exit 12
    done
    command -v go >/dev/null
    command -v shellcheck >/dev/null
    go version >/dev/null
    [ "$(go env GOCACHE)" = /srv/cache/go-build ]
    [ "$(go env GOMODCACHE)" = /srv/cache/go-mod ]
    [ "$(go env GOTOOLCHAIN)" = auto ]
  '
}
check_guest_state

# Force one safe, test-owned cache ownership drift. The public assessment must detect it,
# and its confirmed apply must restore the declared owner before the idempotency check.
incus exec "$instance" --project "$project" -- chown root:root /srv/cache/go-build
drift_owner="$(incus exec "$instance" --project "$project" -- stat -c '%u' /srv/cache/go-build)"
[ "$drift_owner" = 0 ] || die 'fixture did not create the expected cache ownership drift'
repair_log="$STATE/provision-repair.out"
yard provision subyard-dev --yes > "$repair_log"
grep -Fq 'provisioning subyard-dev' "$repair_log" \
  || die 'public provision did not detect and repair cache ownership drift'
check_guest_state

before="$(incus exec "$instance" --project "$project" -- stat -c '%u:%g:%Y' \
  /srv/cache/go-build /srv/cache/go-mod /home/dev/.config/go)"
noop_log="$STATE/provision-noop.out"
yard provision subyard-dev --yes > "$noop_log"
if grep -Fq 'provisioning subyard-dev' "$noop_log"; then
  die 'converged profile provision ran its apply hook again'
fi
after="$(incus exec "$instance" --project "$project" -- stat -c '%u:%g:%Y' \
  /srv/cache/go-build /srv/cache/go-mod /home/dev/.config/go)"
[ "$before" = "$after" ] || die 'converged profile check changed cache directory state'
check_guest_state

printf 'ok: subyard-dev init, provisioning repair, and idempotency\n'
