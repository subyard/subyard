#!/usr/bin/env bash
# Real-host regression for resource-only init and bind wrapper cleanup.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
YARD_NAME=''

die() { printf 'bind-resource-profile-e2e: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
for command in go incus jq sudo sg; do
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
  # First init can enroll this user, but cannot update its parent shell's groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"
    sg incus-admin -c "exec $command"
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
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-bind-resource.* ]] \
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
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"

YARD_BUILD_VERSION=0.7.2 "$ROOT/dev/build-engine.sh" --force
port=$((35000 + ($$ % 15000)))
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
SSH_PORT=$port
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=orca
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

source="$STATE/host/live-bind"
mkdir -p "$source"
printf 'host source must survive\n' > "$source/marker"

yard init --yes
yard start --yes

project="subyard-$YARD_NAME"
instance="yard-$YARD_NAME"

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
profile=fixture-provision
[ ! -e "$ROOT/config/profiles/$profile" ] || die 'provision fixture already exists'
mkdir "$ROOT/config/profiles/$profile"
mkdir -p "$STATE/host/provision-prerequisite"
printf 'ready\n' > "$STATE/host/provision-prerequisite/ready"
printf 'PROFILE_NAME=%s\nYARD_MOUNTS=provision-prerequisite:/mnt/provision-prerequisite:ro:0755\n' \
  "$profile" > "$ROOT/config/profiles/$profile/profile.conf"
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
chmod 0755 "$ROOT/config/profiles/$profile/provision.sh"
yard provision "$profile" --yes
grep -Fq 'orca fixture-provision' "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" \
  || die 'provision lost profile selection'
incus exec "$instance" --project "$project" -- test -f /var/lib/subyard-fixture-provision
# A converged repeat has no prompt and must not run the non-repeatable fixture hook.
yard provision "$profile" </dev/null
printf 'ok: resource-only init, bind detach and generic profile activation\n'
