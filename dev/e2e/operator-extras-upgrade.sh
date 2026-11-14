#!/usr/bin/env bash
# Published 0.27.0 yard with an operator-scope YARD_DEVICES -> frozen candidate on one
# broker-owned disposable VM: the candidate ignores and discloses the assignment, the init
# plan names the owned device the extras stage detaches, and config unset removes the line.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OLD_VERSION=0.27.0
OLD_INSTALLER_SHA256=cbf54fbfc846c2e936c53a0d7efa6366f83220e906dfb893f0671a057b29400d
OLD_BUNDLE_SHA256=b339acd76299a7c301d79e502459d9f4ab6e8d6f486c83e76c53aff1f6bc5e61
if [ "${1:-}" = --preflight ]; then
  bash -n "$0"
  TMPDIR=/tmp go test ./internal/config ./internal/configsync ./internal/adapters/reconcileruntime ./internal/cli -run 'ProfileOnly|IgnoredProfileOnly|TestExtrasDetachPlanned|TestInitPlanDisclosesExtrasDetach|TestInitExtrasDetachPreview|TestUpdateTypedConfirmation' -count=1
  exit
fi
[ "${SUBYARD_E2E_VM:-}" = 1 ] || { printf 'operator-extras-upgrade: run through dev/agent-e2e.sh --vm 1\n' >&2; exit 2; }
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] \
  || { printf 'operator-extras-upgrade: a frozen candidate bundle is required\n' >&2; exit 2; }
CANDIDATE_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
STATE="$(mktemp -d /var/tmp/subyard-operator-extras.XXXXXX)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?broker lease required}"
printf '%s\n' "$RUN_ID" > "$STATE/.fixture-run"
name="extras-${STATE##*.}"
name="${name,,}"
project="subyard-$name" instance="yard-$name" launcher=''
host_config=''
device=yx-dev-kvm

die() { printf 'operator-extras-upgrade: %s\n' "$*" >&2; exit 2; }
pass() { printf 'PASS: %s\n' "$*"; }
incus() { sudo -n /usr/bin/incus "$@"; }
owner() {
  local command
  command=$(python3 -c 'import shlex,sys; print(shlex.join(sys.argv[1:]))' "$@")
  sg incus-admin -c "exec $command"
}
yard() { owner "$launcher" -Y "$name" "$@"; }
# Presence is decided only from a device list that was read; a failed read stops the lane.
attached() {
  local devices
  devices="$(incus config device list "$instance" --project "$project")" \
    || die "cannot read the device list of $instance"
  grep -Fxq "$device" <<<"$devices"
}
cleanup() {
  local original=$? cleanup_exit=0
  trap - EXIT
  set +e
  if [ "$original" != 0 ]; then
    for log in "$STATE"/*.log; do
      [ -f "$log" ] || continue
      printf '%s (last 30 lines):\n' "${log##*/}"
      tail -n 30 "$log"
    done
  fi
  if [ -n "$launcher" ]; then
    # Keep the shared platform: another registered yard stops teardown at this yard.
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64996\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    yard teardown --yes || cleanup_exit=3
    if incus project show "$project" >/dev/null 2>&1; then cleanup_exit=3; fi
  fi
  if [ "$cleanup_exit" = 0 ] && [ ! -L "$STATE" ] && [ "$(cat "$STATE/.fixture-run")" = "$RUN_ID" ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_exit=3
  fi
  printf 'operator-extras-upgrade: original_exit=%s cleanup_exit=%s\n' "$original" "$cleanup_exit"
  [ "$original" != 0 ] || original=$cleanup_exit
  exit "$original"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

sudo -n true || die 'passwordless sudo is required on the disposable VM'
[ -c /dev/kvm ] || die "the VM has no /dev/kvm for the operator device"
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
export SUBYARD_OPERATOR_HOME="$STATE/home" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/runtime" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH
STORAGE_PATH="$(incus storage get default source)"
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
host_config="$SUBYARD_CONFIG_HOME/config.env"

# Exact published runtime, installed through its own public installer.
curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-all-errors --retry-delay 2 --connect-timeout 20 --max-time 300 \
  "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" -o "$STATE/subyard-install.sh"
[ "$(sha256sum "$STATE/subyard-install.sh" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
  || die 'published installer checksum changed'
owner env YARD_RELEASE_VERSION="$OLD_VERSION" YARD_RELEASE_CACHE="$STATE/releases" YARD_BIN_DIR="$STATE/bin" \
  YARD_SHELL_RC="$STATE/bashrc" YARD_LOGIN_RC="$STATE/profile" \
  bash "$STATE/subyard-install.sh" --version "$OLD_VERSION" --yes >"$STATE/install.log" 2>&1 \
  || die 'published install failed'
archive="$(find "$STATE/releases" -name "subyard-$OLD_VERSION-linux-amd64.tar.gz" -type f -print -quit)"
[ -n "$archive" ] && [ "$(sha256sum "$archive" | awk '{print $1}')" = "$OLD_BUNDLE_SHA256" ] \
  || die 'published runtime archive checksum changed'
launcher="$STATE/bin/yard"
[ "$(owner "$launcher" --version)" = "yard $OLD_VERSION" ] || die 'published runtime is not active'

# The operator assignment the published release applies through a selected profile that
# does not assign YARD_DEVICES itself, and that the candidate ignores.
printf 'YARD_DEVICES=kvm\n' > "$host_config"
chmod 0600 "$host_config"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
port=$((36000 + $$ % 10000))
cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SSH_PORT=$port
YARD_KIND=container
YARD_IMAGE=images:debian/13/cloud
YARD_IMAGE_FALLBACK=images:debian/13/cloud
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=git-fetch
DEV_USER=dev
DEV_UID=1001
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
LIMITS_CPU=1
LIMITS_MEMORY=1GiB
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
yard init --yes >"$STATE/published-init.log" 2>&1 || die 'published init failed'
yard start --yes >"$STATE/published-start.log" 2>&1 || die 'published start failed'
attached || die "published $OLD_VERSION did not attach $device from the operator YARD_DEVICES"
pass "published $OLD_VERSION attached $device from host-scope YARD_DEVICES"

owner env YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/release" \
  "$launcher" update --version "$CANDIDATE_VERSION" --yes >"$STATE/update.log" 2>&1 \
  || die 'candidate update failed'
[ "$(owner "$launcher" --version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate is not active'
attached || die 'the update itself detached the device before any init'
pass "updated to $CANDIDATE_VERSION without refusing the ignored assignment"

repair="yard -Y $name config unset YARD_DEVICES --scope host --local"
disclosure="ignored: YARD_DEVICES in $host_config:1 is ignored: only environment profiles declare it; remove it with $repair"
yard config show >"$STATE/config-show.log" 2>&1 || die 'config show failed'
grep -Fxq "$disclosure" "$STATE/config-show.log" || die 'config show did not disclose the ignored assignment'
yard config show YARD_DEVICES >"$STATE/config-show-setting.log" 2>&1 || die 'config show YARD_DEVICES failed'
grep -Fxq "$disclosure" "$STATE/config-show-setting.log" || die 'config show YARD_DEVICES did not disclose it'
owner env YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/release" \
  "$launcher" update --check --json --version "$CANDIDATE_VERSION" >"$STATE/update-check.json" 2>"$STATE/update-check.log" \
  || die 'update --check failed'
# A host assignment is reported once across local yards; any yard's unset removes it.
jq -e --arg path "$host_config" '[.configuration[]? | select(.setting == "YARD_DEVICES"
  and .scope == "host" and .path == $path and .line == 1
  and (.repair | test("^yard -Y [a-z0-9-]+ config unset YARD_DEVICES --scope host --local$")))] | length == 1' \
  "$STATE/update-check.json" >/dev/null || {
  jq -c '.configuration' "$STATE/update-check.json" >&2 || head -c 2000 "$STATE/update-check.json" >&2
  die 'update --check --json did not report the ignored assignment'
}
pass 'config show, config show YARD_DEVICES and update --check --json disclose the assignment and its repair'

# Declined init only plans: its confirmation names the device loss and nothing changes.
# The consequences are shown at the interactive prompt, so answer it on a terminal.
command -v script >/dev/null || die 'util-linux script is required'
plan_command="$(python3 -c 'import shlex,sys; print(shlex.join(sys.argv[1:]))' "$launcher" -Y "$name" init)"
if printf 'n\n' | owner script -qefc "$plan_command" /dev/null >"$STATE/init-plan.log" 2>&1; then
  die 'init applied without confirmation'
fi
grep -Fq 'operation declined' "$STATE/init-plan.log" || die 'init did not reach its confirmation'

grep -Fq "  $disclosure" "$STATE/init-plan.log" || die 'the init plan did not disclose the ignored assignment'
grep -Fq "detaches devices no selected profile requests: $device" "$STATE/init-plan.log" \
  || die "the init plan did not name $device"
attached || die 'an unconfirmed init detached the device'
pass "the init plan names $device before confirmation and changes nothing"

yard config unset YARD_DEVICES --scope host --local --yes >"$STATE/unset.log" 2>&1 || die 'config unset failed'
if [ -e "$host_config" ] && grep -q '^YARD_DEVICES=' "$host_config"; then
  die 'config unset left the assignment'
fi
yard config show >"$STATE/config-show-after.log" 2>&1 || die 'config show after unset failed'
! grep -q '^ignored: ' "$STATE/config-show-after.log" || die 'the removed assignment is still disclosed'
pass 'the printed config unset repair removed the assignment'

yard init --yes >"$STATE/init.log" 2>&1 || die 'init after unset failed'
! attached || die "init did not detach $device"
pass "init detached $device that no selected profile requests"
