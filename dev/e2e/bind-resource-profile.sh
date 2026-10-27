#!/usr/bin/env bash
# Real-host regression for minimal init, resource limits and bind wrapper cleanup.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
YARD_NAME=''
mode=normal
case "$#:${1:-}" in
  0:) ;;
  1:--producer-reboot) mode=producer-reboot ;;
  *) printf 'Usage: bind-resource-profile.sh [--producer-reboot]\n' >&2; exit 2 ;;
esac

die() { printf 'bind-resource-profile-e2e: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
for command in go incus jq sudo sg; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
[ ! -e "$ROOT/.subyard-acceptance/candidate.json" ] && [ ! -L "$ROOT/.subyard-acceptance/candidate.json" ] \
  || die 'this synthetic-profile fixture requires a plain source bundle'

incus() {
  local -a prefix=()
  [ "$mode" != producer-reboot ] || prefix=(timeout --signal=TERM --kill-after=2 15)
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    "${prefix[@]}" sudo -n /usr/bin/incus "$@"
  else
    "${prefix[@]}" /usr/bin/incus "$@"
  fi
}
yard() {
  local program="$ROOT/.build/yard"
  [ "$mode" != producer-reboot ] || program="$fixture_launcher"
  # First init can enroll this user, but cannot update its parent shell's groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$program" -Y "$YARD_NAME" "$@"
    sg incus-admin -c "exec $command"
  else
    "$program" -Y "$YARD_NAME" "$@"
  fi
}

producer_reboot_check() {
  local output="$STATE/profile-list.json"
  install -m 0600 /dev/null "$output"
  timeout --signal=TERM --kill-after=2 30 "$fixture_launcher" -Y "$YARD_NAME" profile list --json \
    >"$output" 2>/dev/null || return "$?"
  jq -e '[.profiles[] | select(.name == "fixture-provision")] | length == 1 and .[0].convergence == "current"' \
    "$output" >/dev/null
}
producer_reboot_probe() {
  local before_boot after_boot cycle
  yard provision "$profile" --yes || return "$?"
  producer_reboot_check || return "$?"
  # Real block-backed /srv is required to exercise the reboot identity boundary.
  incus exec "$instance" --project "$project" -- sh -ceu \
    'mountpoint -q /srv; test "$(stat -c %d /srv)" != "$(stat -c %d /var/lib/subyard)"' || return "$?"
  before_boot="$(incus exec "$instance" --project "$project" -- cat /proc/sys/kernel/random/boot_id)" || return "$?"
  yard stop --yes || return "$?"
  yard start --yes || return "$?"
  after_boot="$(incus exec "$instance" --project "$project" -- cat /proc/sys/kernel/random/boot_id)" || return "$?"
  [ -n "$before_boot" ] && [ -n "$after_boot" ] && [ "$before_boot" != "$after_boot" ] || return 1
  producer_reboot_check || return "$?"
  for cycle in 1 2; do
    yard init --yes || return "$?"
    producer_reboot_check || return "$?"
    printf 'producer_reboot checkpoint=%s convergence=current\n' "$cycle"
    if [ "$cycle" = 1 ]; then
      bash "$ROOT/dev/e2e/vm-page-reporting.sh" "$project" "$instance" || return "$?"
    fi
  done
  printf 'producer_reboot=result-not-reproduced reboots=1 init_checkpoints=2 diagnostic_only=true full_release_gate=false\n'
}

cleanup() {
  local rc=$? teardown_failed=0 diagnostics
  trap - EXIT INT TERM
  set +e
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    if [ -z "$STATE" ] || [[ "$STATE" != /var/tmp/subyard-bind-resource.* ]] \
      || [ ! -f "$STATE/.marker" ] \
      || [ "$(<"$STATE/.marker")" != subyard-bind-resource-e2e-v1 ]; then
      printf 'bind-resource-profile-e2e: cannot retain teardown diagnostics in an unmarked fixture\n' >&2
      rc=3
      teardown_failed=1
    else
      diagnostics="$STATE/.teardown-diagnostics.log"
      install -m 0600 /dev/null "$diagnostics"
      if ! yard teardown --yes >"$diagnostics" 2>&1; then
        rc=3
        teardown_failed=1
        printf 'bind-resource-profile-e2e: native teardown failed; preserving marker-owned fixture diagnostics\n' >&2
        python3 - "$diagnostics" <<'PYTHON'
import re
import sys
from pathlib import Path

lines = Path(sys.argv[1]).read_text(errors="replace").splitlines()[-80:]
relevant = [line for line in lines if re.search(r"yard:|plan_stale|\bError:|\bERROR:|\[FAIL\]", line)]
for line in relevant[-20:]:
    line = re.sub(r"[A-Za-z0-9_+/=-]{32,}", "[redacted]", line)
    print(line[:512], file=sys.stderr)
PYTHON
      fi
    fi
  fi
  if [ "$teardown_failed" = 0 ] && [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-bind-resource.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-bind-resource-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-bind-resource.XXXXXX)"
printf '%s\n' subyard-bind-resource-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="bind-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/runtime"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
"$ROOT/dev/build-engine.sh"
# Init binds every shipped integration package, including deselected native assets.
"$ROOT/dev/build-profiles.sh"
[ -x "$ROOT/.build/yard" ] || die 'source engine is unavailable'
# Stage the unselected synthetic catalog before packaging.
profile=fixture-provision
[ ! -e "$ROOT/config/profiles/$profile" ] || die 'provision fixture already exists'
mkdir "$ROOT/config/profiles/$profile"
printf 'PROFILE_NAME=%s\n' "$profile" > "$ROOT/config/profiles/$profile/profile.conf"
if [ "$mode" = producer-reboot ]; then
  cat > "$ROOT/config/profiles/$profile/provision.sh" <<'HOOK'
#!/usr/bin/env bash
# subyard-provision-check-v1
set -euo pipefail
marker=/var/lib/subyard-fixture-provision
if [ "${1:-}" = --check ]; then [ -f "$marker" ] && exit 0; exit 10; fi
[ ! -e "$marker" ]
printf 'installed\n' > "$marker"
chmod 0600 "$marker"
HOOK
else
printf 'YARD_MOUNTS=provision-prerequisite:/mnt/provision-prerequisite:ro:0755\n' >> "$ROOT/config/profiles/$profile/profile.conf"
cat > "$ROOT/config/profiles/$profile/provision.sh" <<'HOOK'
#!/usr/bin/env bash
# subyard-provision-check-v1
set -euo pipefail
marker=/var/lib/subyard-fixture-provision
if [ "${1:-}" = --check ]; then
  [ -f /mnt/provision-prerequisite/ready ] && [ -f "$marker" ] && exit 0
  exit 10
fi
[ "$(cat /mnt/provision-prerequisite/ready)" = ready ]
[ ! -e "$marker" ]
printf 'installed\n' > "$marker"
chmod 0600 "$marker"
HOOK
fi
chmod 0755 "$ROOT/config/profiles/$profile/provision.sh"
# Commit the isolated release baseline before creating any yard registration.
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
fixture_launcher="$(release_candidate_install "$ROOT" "$STATE")"
candidate_version="$("$fixture_launcher" --version)"
candidate_version="${candidate_version#yard }"
YARD_BUILD_VERSION="$candidate_version" "$ROOT/dev/build-engine.sh" --force
cmp -s "$ROOT/.build/yard" "$YARD_RUNTIME_ROOT/current/bin/yard-engine" \
  || die 'source and installed engines differ'
printf 'bind-resource-profile-e2e: engine-identity-preflight=PASS\n'
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"
port=$((35000 + ($$ % 15000)))
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
SSH_PORT=$port
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
# Diagnostic VM topology uses the ordinary public configuration path.
if [ "$mode" = producer-reboot ]; then
  printf 'YARD_KIND=vm\nLIMITS_CPU=2\nLIMITS_MEMORY=1GiB\nROOT_DISK_SIZE=10GiB\nSRV_VOLUME_TYPE=block\nSRV_VOLUME_SIZE=2GiB\nVM_FREE_PAGE_REPORTING=1\nHOST_MOUNTS=\nHOST_LINKS=\n' \
    >> "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
  incus list --project default --format=json >/dev/null
fi

source="$STATE/host/live-bind"
mkdir -p "$source"
printf 'host source must survive\n' > "$source/marker"

yard init --yes || {
  rc=$?
  "$fixture_launcher" migrate --check --json | jq '{outcome, decisions}' >&2 || true
  exit "$rc"
}
yard start --yes

project="subyard-$YARD_NAME"
instance="yard-$YARD_NAME"
if [ "$mode" = producer-reboot ]; then
  producer_reboot_probe
  exit
fi

# Existing container limits must reconcile through the product without replacing state.
incus exec "$instance" --project "$project" -- sh -ceu \
  'printf "retained state\n" > /srv/resource-limits-marker'
assert_limits() {
  [ "$(incus config get "$instance" limits.cpu --project "$project")" = "$1" ] \
    && [ "$(incus config get "$instance" limits.memory --project "$project")" = "$2" ] \
    || die 'existing container resource limits did not converge'
  incus exec "$instance" --project "$project" -- grep -Fxq 'retained state' /srv/resource-limits-marker
}
yard config set LIMITS_CPU 1 --scope yard --yes
yard config set LIMITS_MEMORY 512MiB --scope yard --yes
yard init --yes
assert_limits 1 512MiB
init_pid="$(incus query "/1.0/instances/$instance/state?project=$project" | jq -er '.pid | select(. > 0)')"
yard init </dev/null
assert_limits 1 512MiB
[ "$(incus query "/1.0/instances/$instance/state?project=$project" | jq -er '.pid | select(. > 0)')" = "$init_pid" ] \
  || die 'converged init restarted the container'
yard config set LIMITS_CPU 2 --scope yard --yes
yard config set LIMITS_MEMORY 768MiB --scope yard --yes
yard init --yes
assert_limits 2 768MiB
yard stop --yes
yard config set LIMITS_CPU 1 --scope yard --yes
yard config set LIMITS_MEMORY 1GiB --scope yard --yes
yard init --yes
[ "$(incus config get "$instance" limits.cpu --project "$project")" = 1 ] \
  && [ "$(incus config get "$instance" limits.memory --project "$project")" = 1GiB ] \
  || die 'stopped container limits did not converge'
incus list "$instance" --project "$project" --format csv -c s | grep -Fxq STOPPED \
  || die 'resource limit reconciliation lost stopped power intent'
yard start --yes
assert_limits 1 1GiB
yard config set LIMITS_CPU '' --scope yard --yes
yard config set LIMITS_MEMORY '' --scope yard --yes
yard init </dev/null
assert_limits 1 1GiB
printf 'ok: existing container limits, repeat init and stopped power intent\n'

# Incus numeric UID/GID exec does not initialize the account's supplementary groups.
# Shell sessions must retain the same identity and Docker access as a login session.
yard shell -- sh -c '
  set -eu
  test "$(id -un)" = dev
  test "$(id -g)" = "$(id -g dev)"
  test "$(id -G | tr " " "\n" | sort -n)" = "$(id -G dev | tr " " "\n" | sort -n)"
  docker info --format "{{.ServerVersion}}" >/dev/null
'

yard bind "$source" --name live-bind --target yard --yes
yard remove live-bind --yes
[ "$(<"$source/marker")" = 'host source must survive' ] \
  || die 'normal bind removal changed the host source'
if incus exec "$instance" --project "$project" -- test -e /srv/workspaces/live-bind; then
  die 'normal bind removal left its generated workspace wrapper'
fi

# A generic profile must be selected and its mount requirement reconciled before
# its hook runs. This fixture is created only in the disposable runner checkout.
mkdir -p "$STATE/host/provision-prerequisite"
printf 'ready\n' > "$STATE/host/provision-prerequisite/ready"
yard provision "$profile" --yes
grep -Fxq "ENVIRONMENT_PROFILES='fixture-provision'" "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" \
  || die 'provision lost profile selection'
incus exec "$instance" --project "$project" -- test -f /var/lib/subyard-fixture-provision
# A converged repeat has no prompt and must not run the non-repeatable fixture hook.
yard provision "$profile" </dev/null
printf 'ok: minimal init, bind detach and generic profile activation\n'
