#!/usr/bin/env bash
# Real predecessor ownership and normal update on one fresh allocated owner VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
BASELINE_REVISION=5057f2f72a76dd503e720841406524ba93b2aad8
BASELINE_VERSION=0.19.0-openlit.migration
STATE=''
SOURCE=''
YARD_NAME=''
driver_pid=''
driver_input=''
driver_output=''
# shellcheck source=config/profiles/openlit/tests/e2e/candidate.sh
. "$ROOT/config/profiles/openlit/tests/e2e/candidate.sh"
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
die() { printf 'openlit-migration: %s\n' "$*" >&2; exit 2; }
[ "$#" = 0 ] || die 'migration has no guest arguments'
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] || die 'requires a disposable allocated owner VM'
[ "$(id -un)" = dev ] || die 'requires allocated dev user'
for command in go git incus jq sudo python3 rg sha256sum; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on disposable VM'
python3 - "${SUBYARD_E2E_MEMORY_PER_VM:-}" <<'PY'
import re,sys
value=re.fullmatch(r'([1-9][0-9]*)(MiB|GiB)',sys.argv[1])
assert value and int(value[1])*(1024 if value[2]=='GiB' else 1)>=8192, 'migration requires at least8GiB'
PY
incus() { openlit_candidate_command /usr/bin/incus "$@"; }
candidate() { openlit_candidate_command "$STATE/runtime/current/bin/yard" "$@"; }
yard() { candidate -Y "$YARD_NAME" "$@"; }
driver() {
  local action="$1" reply
  printf '{"action":"%s"}\n' "$action" >&"$driver_input"
  IFS= read -r -t 300 -u "$driver_output" reply || die "native process driver unavailable at $action"
  printf 'openlit_migration_action=%s result=%s\n' "$action" "$reply"
  jq -e '.ok == true' <<< "$reply" >/dev/null || die "native proof failed at $action"
}
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$driver_pid" ]; then
    # EOF closes the control stream; the driver's finally block owns its exact
    # process handles and foreign fixture container. No process-name killing.
    if [ -n "$driver_input" ]; then exec {driver_input}>&-; fi
    if ! wait "$driver_pid"; then rc=3; fi
  fi
  if [ -n "$YARD_NAME" ] && [ -f "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    if yard teardown --yes > "$STATE/cleanup.log" 2>&1; then
      printf 'openlit_migration_cleanup=yard-teardown result=passed\n' >&2
    else
      printf 'openlit_migration_cleanup=yard-teardown result=failed\n' >&2
      rc=3
    fi
  fi
  if [ -n "$SOURCE" ] && [[ "$SOURCE" = /tmp/subyard-openlit-predecessor.* ]] \
    && [ -f "$SOURCE/.fixture-marker" ] && [ "$(<"$SOURCE/.fixture-marker")" = openlit-migration-source-v1 ]; then
    rm -rf -- "$SOURCE" || rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-openlit-migration.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = openlit-profile-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
STATE="$(mktemp -d /var/tmp/subyard-openlit-migration.XXXXXX)"
printf '%s\n' openlit-profile-e2e-v1 > "$STATE/.marker"
chmod 0600 "$STATE/.marker"
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export YARD_RUNTIME_ROOT="$STATE/runtime" YARD_RELEASE_CACHE="$STATE/releases"
export YARD_BIN_DIR="$STATE/bin" YARD_SHELL_RC="$STATE/bashrc" YARD_LOGIN_RC="$STATE/profile"
unset YARD_ENGINE_PATH
# Require the transported final candidate. A fallback source build cannot bind
# this migration's target to the controller's frozen candidate receipt.
final_engine="$(release_candidate_prepare "$ROOT")" || die 'verified transported final candidate is required'
final_release="$ROOT/.subyard-acceptance/release"
final_version="$(jq -er '.version | select(type == "string")' "$ROOT/.subyard-acceptance/candidate.json")"
case "$final_version" in ''|*[!A-Za-z0-9._+-]*) die 'unsafe final candidate version' ;; esac
[ "$final_version" != "$BASELINE_VERSION" ] || die 'final candidate must differ from predecessor'
[ -x "$final_engine" ] || die 'final candidate engine is missing'
SOURCE="$(mktemp -d /tmp/subyard-openlit-predecessor.XXXXXX)"
printf '%s\n' openlit-migration-source-v1 > "$SOURCE/.fixture-marker"
chmod 0600 "$SOURCE/.fixture-marker"
printf 'openlit_migration_stage=pinned-public-predecessor-fetch\n'
# No user/global Git credentials, hooks or fallback refs. The only source is the
# public immutable revision; network/build failure leaves this lane incomplete.
env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
  timeout --signal=TERM --kill-after=30 180 bash -euo pipefail -c '
    git init -q "$1/repo"
    git -C "$1/repo" -c credential.helper= -c core.hooksPath=/dev/null fetch --quiet --depth=1 https://github.com/Subyard/Subyard.git "$2"
    git -C "$1/repo" -c core.hooksPath=/dev/null checkout --quiet --detach FETCH_HEAD
    [ "$(git -C "$1/repo" rev-parse HEAD)" = "$2" ]
  ' fixture "$SOURCE" "$BASELINE_REVISION" || die 'bounded pinned public predecessor fetch failed'
baseline_release="$STATE/baseline-release"
printf 'openlit_migration_stage=predecessor-package source=%s\n' "$BASELINE_REVISION"
timeout --signal=TERM --kill-after=30 600 "$SOURCE/repo/dev/package-engine.sh" \
  --version "$BASELINE_VERSION" --output-dir "$baseline_release" || die 'bounded predecessor package build failed'
# Product platform setup and bootstrap own initial installation and activation.
openlit_candidate_platform "$ROOT" "$STATE" "$SOURCE/repo"
openlit_candidate_command env YARD_RELEASE_BASE_URL="file://$baseline_release" \
  bash "$baseline_release/subyard-install.sh" --version "$BASELINE_VERSION" --yes
candidate migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="dev-e2e-migration-$token"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
YARD_KIND=container
SSH_PORT=$((35000 + ($$ % 15000)))
CODING_TOOL_INTEGRATIONS="codex"
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
printf 'openlit_migration_stage=authentic-predecessor-init\n'
yard init --yes
yard start --yes
instance="yard-$YARD_NAME"
project="subyard-$YARD_NAME"
incus exec "$instance" --project "$project" -- bash -euo pipefail -c 'command -v codex >/dev/null; codex-policy-check; runuser -u dev -- codex --version'
printf 'openlit_migration_stage=authentic-predecessor-enable\n'
# The real predecessor owns only log/metric leaves; its normal cold-image240s
# guard remains in charge. Do not patch its receiver or ownership ledger.
yard profile enable openlit --yes
incus exec "$instance" --project "$project" -- python3 -c '
import os,sys
path="/tmp/subyard-openlit-migration-process.py"
fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,"w") as out: out.write(sys.stdin.read())
' < "$ROOT/config/profiles/openlit/tests/e2e/migration-process.py"
coproc NATIVE_DRIVER { incus exec "$instance" --project "$project" -- python3 -B /tmp/subyard-openlit-migration-process.py; }
driver_pid=$NATIVE_DRIVER_PID
# Duplicate coproc descriptors so Bash keeps them valid after an early child exit.
exec {driver_output}<&"${NATIVE_DRIVER[0]}"
exec {driver_input}>&"${NATIVE_DRIVER[1]}"
original_driver_input=${NATIVE_DRIVER[1]}
exec {original_driver_input}>&-
driver setup
printf 'openlit_migration_compatibility_scope=declared-layout actual-deployed-helper=UNKNOWN\n'
driver old-turn
printf 'openlit_migration_stage=stage-exact-final-cache\n'
# Supported publish-only bootstrap downloads/verifies the exact candidate into
# its ordinary cache without activation. The predecessor then owns the update.
openlit_candidate_command env YARD_RELEASE_BASE_URL="file://$final_release" \
  bash "$final_release/subyard-install.sh" --version "$final_version" --publish-only --yes
printf 'openlit_migration_stage=normal-offline-update\n'
candidate update --version "$final_version" --offline --yes
driver adoption
driver old-turn
driver fresh-turn
driver retain
driver retire
printf 'openlit_migration_stage=fixed-point-update\n'
driver before-repeat
candidate update --version "$final_version" --offline --yes
driver after-repeat
candidate migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null
yard profile disable openlit --yes
driver disabled
yard profile enable openlit --yes
driver reenabled
driver close
exec {driver_input}>&-
driver_input=''
wait "$driver_pid"
driver_pid=''
printf 'ok: normal predecessor update, genuine old/new native traces, automatic retirement and post-adoption lifecycle\n'
printf 'openlit_migration_compatibility_scope=declared-layout actual-deployed-helper=UNKNOWN\n'
