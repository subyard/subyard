#!/usr/bin/env bash
# Real-host provisioning acceptance for openlit on the allocated owner VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
CANDIDATE_ENGINE=''

die() { printf 'openlit-e2e: %s\n' "$*" >&2; exit 2; }
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
  local engine="${CANDIDATE_ENGINE:-$ROOT/.build/yard}" command
  # Init refreshes its child session; the fixture's parent keeps the old groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' "$engine" -Y "$YARD_NAME" "$@"
    sg incus-admin -c "exec $command"
  else
    "$engine" -Y "$YARD_NAME" "$@"
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
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-openlit.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = openlit-profile-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-openlit.XXXXXX)"
printf '%s\n' openlit-profile-e2e-v1 > "$STATE/.marker"
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
CODING_TOOL_INTEGRATIONS="claude codex"
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

printf 'openlit_e2e_stage=default-off-init\n'
yard init --yes
instance="yard-$YARD_NAME"
project="subyard-$YARD_NAME"
incus exec "$instance" --project "$project" -- test ! -e /var/lib/subyard/openlit
printf 'openlit_e2e_stage=enable\n'
yard profile enable openlit --yes
# The guest uses only synthetic prompts and local fake model APIs; no account credentials.
tar -C "$ROOT/config/profiles/openlit/tests/e2e" -cf - native-signals.py runtime-refresh.py | \
  incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
    stage="$(mktemp -d /tmp/subyard-openlit-signals.XXXXXX)"
    trap '\''rm -rf -- "$stage"'\'' EXIT
    tar -xf - -C "$stage"
    python3 "$stage/native-signals.py"
    python3 "$stage/runtime-refresh.py"
  '
printf 'openlit_e2e_stage=repeat-init-and-configs\n'
yard init --yes
yard config apply --yes
yard profile status openlit --json >/dev/null
printf 'openlit_e2e_stage=disable\n'
yard profile disable openlit --yes
incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
  ! systemctl is-active --quiet subyard-openlit.service
  ! systemctl is-enabled --quiet subyard-openlit.service
  python3 - <<"PYCODE"
import json,tomllib
from pathlib import Path
state=json.loads(Path("/var/lib/subyard/openlit/native.json").read_text())
assert state["agents"] == {}
claude=json.loads(Path("/home/dev/.claude/settings.json").read_text())
codex=tomllib.loads(Path("/home/dev/.codex/config.toml").read_text())
assert "CLAUDE_CODE_ENABLE_TELEMETRY" not in claude.get("env",{})
assert "exporter" not in codex.get("otel",{})
assert codex["model"] == "fixture-custom-preserved"
assert claude["env"]["OPENLIT_E2E_CUSTOM"] == "preserved"
PYCODE
'
yard init --yes
printf 'ok: OpenLIT default off, enable, runtime CAS update/rollback, repeat init/config apply, native logs+metrics ingestion and disable\n'
