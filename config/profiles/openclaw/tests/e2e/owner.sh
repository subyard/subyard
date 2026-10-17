#!/usr/bin/env bash
# Real-host OpenClaw toolchain and shared-cache acceptance on an allocated disposable VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
YARD_BIN=''
TOKEN=''
MARKER=openclaw-profile-e2e-v1
STAGING_TRACE_DIR=''
STAGING_ORIGINAL_PATH=''
STAGING_CAPTURE_ACTIVE=0
STAGING_STEP=unknown
STAGING_STEP_STATE=unknown

die() { printf 'openclaw-e2e: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
[ "$(id -un)" = dev ] || die 'run as the allocated dev user'
for command in go incus jq sudo timeout; do
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
  # Init may update account membership without refreshing this fixture's groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$YARD_BIN" -Y "$YARD_NAME" "$@"
    sg incus-admin -c "$command"
  else
    "$YARD_BIN" -Y "$YARD_NAME" "$@"
  fi
}

# shellcheck source=dev/e2e/lib-owner-project-contract.sh
. "$ROOT/dev/e2e/lib-owner-project-contract.sh"

# Fixture-only diagnostics: fixed labels and a bounded recent-probe ring, never native data.
enable_staging_capture() {
  STAGING_ORIGINAL_PATH="$PATH"
  STAGING_TRACE_DIR="$STATE/staging-diagnostics"
  install -d -m 0700 "$STAGING_TRACE_DIR" "$STAGING_TRACE_DIR/bin"
  printf 'unknown\n' > "$STAGING_TRACE_DIR/step"
  chmod 0600 "$STAGING_TRACE_DIR/step"
  cat > "$STAGING_TRACE_DIR/bin/incus" <<'INCUS_DELEGATE'
#!/usr/bin/env bash
# Delegate unchanged native I/O; only fixed operation classes enter the private ring.
diag="${BASH_SOURCE[0]%/*}/.."
step=unknown
IFS= read -r step 2>/dev/null < "$diag/step" || step=unknown
case "$step" in
 first-start|running-status|repeat-start|stop|stopped-status|existing-up|expired-down|expired-up|down|down-status|destroy|destroy-status) ;;
 *) step=unknown ;;
esac
probe=other
case "${1:-}" in
 info|list) probe="$1" ;;
 exec)
   command=("$@")
   for ((i=1; i<${#command[@]}; i++)); do
     [ "${command[i]}" != -- ] || break
   done
   case "${command[i+1]:-}" in
     id) probe=exec-id ;;
     stat) probe=exec-stat ;;
     tar) probe=exec-tar ;;
     sh|bash) probe=exec-shell ;;
     docker)
       case "${command[i+2]:-}" in inspect) probe=docker-inspect ;; exec) probe=docker-exec ;; *) probe=docker-other ;; esac ;;
   esac ;;
esac
start=unknown
IFS=' ' read -r start _ 2>/dev/null < /proc/uptime || start=unknown
slot="$diag/probe.$(( $$ % 128 ))"
 record_probe() {
   local state="$1" finish="$2" result="$3"
   # Fixed slots bound disk use; collisions may lose history, never imply completion.
   [ ! -L "$slot" ] || return 1
   (umask 077; printf 'step=%s probe=%s pid=%s state=%s start=%s end=%s rc=%s\n' \
     "$step" "$probe" "$$" "$state" "$start" "$finish" "$result" > "$slot" && chmod 0600 "$slot") 2>/dev/null
 }
record_probe start unknown unknown || :
/usr/bin/incus "$@"
rc=$?
finish=unknown
IFS=' ' read -r finish _ 2>/dev/null < /proc/uptime || finish=unknown
record_probe end "$finish" "$rc" || :
exit "$rc"
INCUS_DELEGATE
  chmod 0700 "$STAGING_TRACE_DIR/bin/incus"
  export PATH="$STAGING_TRACE_DIR/bin:$PATH"
  STAGING_CAPTURE_ACTIVE=1
}

staging_step() {
  local step="$1" output="$2"
  shift 2
  case "$step" in
    first-start|running-status|repeat-start|stop|stopped-status|existing-up|expired-down|expired-up|down|down-status|destroy|destroy-status) ;;
    *) die 'unknown staging diagnostic step' ;;
  esac
  STAGING_STEP="$step"
  STAGING_STEP_STATE=start
  printf '%s\n' "$step" > "$STAGING_TRACE_DIR/step"
  chmod 0600 "$STAGING_TRACE_DIR/step"
  printf 'openclaw_e2e_step=%s state=start\n' "$step"
  yard staging "$@" > "$output"
  STAGING_STEP_STATE=end
  printf 'unknown\n' > "$STAGING_TRACE_DIR/step"
  chmod 0600 "$STAGING_TRACE_DIR/step"
  printf 'openclaw_e2e_step=%s state=end\n' "$step"
}

capture_staging_failure() {
  local original="$1" index path record matching=unknown capture_rc=0
  [ -n "$STAGING_TRACE_DIR" ] && [ ! -L "$STAGING_TRACE_DIR" ] \
    && [ "$(stat -c %a "$STAGING_TRACE_DIR")" = 700 ] || return 2
  printf 'openclaw_e2e_failure original_rc=%s last_step=%s step_state=%s history=bounded-recent\n' \
    "$original" "$STAGING_STEP" "$STAGING_STEP_STATE"
  for ((index=0; index<128; index++)); do
    path="$STAGING_TRACE_DIR/probe.$index"
    [ -e "$path" ] || continue
    if [ -L "$path" ] || [ ! -f "$path" ] || [ "$(stat -c %a "$path")" != 600 ] \
      || [ "$(wc -c < "$path")" -gt 256 ]; then capture_rc=2; continue; fi
    record="$(cat "$path")"
    if [[ "$record" =~ ^step=(first-start|running-status|repeat-start|stop|stopped-status|existing-up|expired-down|expired-up|down|down-status|destroy|destroy-status|unknown)\ probe=(info|list|exec-id|exec-stat|exec-tar|exec-shell|docker-inspect|docker-exec|docker-other|other)\ pid=[0-9]+\ state=(start|end)\ start=([0-9]+\.[0-9]+|unknown)\ end=([0-9]+\.[0-9]+|unknown)\ rc=([0-9]+|unknown)$ ]]; then
      printf 'openclaw_e2e_native %s\n' "$record"
      case "$record" in "step=$STAGING_STEP "*' state=start '*) matching=pending ;; esac
    else capture_rc=2; fi
  done
  printf 'openclaw_e2e_capture native_match=%s capture_rc=%s\n' "$matching" "$capture_rc"
  return "$capture_rc"
}

cleanup() {
  local rc=$? original capture_rc=0 cleanup_rc=0
  original="$rc"
  trap - EXIT INT TERM
  set +e
  if [ -n "$STAGING_ORIGINAL_PATH" ]; then export PATH="$STAGING_ORIGINAL_PATH"; fi
  if [ "$original" != 0 ] && [ "$STAGING_CAPTURE_ACTIVE" = 1 ]; then
    capture_staging_failure "$original" || capture_rc=$?
  fi
  if [ -n "$TOKEN" ]; then
    project_contract_clean_tree "/tmp/subyard-p0-project-$TOKEN" "$MARKER" || cleanup_rc=3
  fi
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64997\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    yard teardown --yes >/dev/null 2>&1 || cleanup_rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-openclaw.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = openclaw-profile-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_rc=3
  fi
  printf 'openclaw_e2e_result original_rc=%s capture_rc=%s cleanup_rc=%s\n' "$original" "$capture_rc" "$cleanup_rc"
  [ "$cleanup_rc" = 0 ] || rc="$cleanup_rc"
  [ "$rc" != 0 ] || rc="$capture_rc"
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-openclaw.XXXXXX)"
printf '%s\n' openclaw-profile-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
TOKEN="$token"
YARD_NAME="openclaw-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/overrides/host"
printf '%s\n' "$(printf '%s' openclaw-e2e-synthetic-denylist-token | sha256sum | awk '{print $1}')" \
  > "$SUBYARD_CONFIG_HOME/overrides/host/prod-fingerprints"
chmod 0600 "$SUBYARD_CONFIG_HOME/overrides/host/prod-fingerprints"

# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
if [ -e "$ROOT/.subyard-acceptance/candidate.json" ] || [ -L "$ROOT/.subyard-acceptance/candidate.json" ]; then
  YARD_BIN="$(release_candidate_prepare "$ROOT")" \
    || die 'packaged release candidate is invalid or unavailable'
else
  "$ROOT/dev/build-engine.sh" --force
  YARD_BIN="$ROOT/.build/yard"
fi

# Activation needs native owner access before any uninitialized yard is registered.
# Reuse the product reconciler, then commit the protected publication reader epoch
# through the supported installer; running a source engine does not establish it.
printf 'openclaw_e2e_stage=release-preflight\n'
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$STATE/incus-foundation"
  export SUBYARD_USER=dev SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
  export SUBYARD_DISPATCHER_PATH="$YARD_BIN"
  export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
  set -a
  # shellcheck source=config/host.env
  . "$ROOT/config/host.env"
  set +a
  bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
)
timeout 10 /usr/bin/incus query /1.0 >/dev/null
export YARD_RUNTIME_ROOT="$STATE/runtime"
unset YARD_ENGINE_PATH
YARD_BIN="$(release_candidate_install "$ROOT" "$STATE")"
"$YARD_BIN" _publication-contracts >/dev/null
printf 'openclaw candidate release-preflight=PASS\n'
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"

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
yard start --yes

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
    for path in /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/playwright; do
      [ -d "$path" ] && [ ! -L "$path" ]
      [ "$(stat -c %u:%g "$path")" = "$expected" ]
    done
    [ -x /usr/local/bin/sy-cache ]
    [ -f /usr/local/bin/npm ] && [ ! -L /usr/local/bin/npm ]
    [ -f /usr/local/bin/npx ] && [ ! -L /usr/local/bin/npx ]
    site_dir="$(/opt/venv/bin/python -c "import sysconfig; print(sysconfig.get_path(\"purelib\"))")"
    [ "$(stat -c %u:%g:%a "$site_dir/subyard_cache.py")" = 0:0:644 ]
    [ "$(stat -c %u:%g:%a "$site_dir/subyard_cache.pth")" = 0:0:644 ]
    pip_cache="$(/opt/venv/bin/python -m pip cache dir)"
    case "$pip_cache" in /srv/cache/pip/linux-*/cpython-*) ;; *) exit 1 ;; esac
    pnpm_store="$(pnpm store path)"
    case "$pnpm_store" in /srv/cache/pnpm/pnpm/linux-*/11.2.2-node-*/*) ;; *) exit 1 ;; esac
    # Separate file descriptions must observe the held lock, while two readers may overlap.
    sy-cache shared -- sh -c "! flock -n -x /srv/cache/.sy-cache.lock true"
    sy-cache exclusive -- sh -c "! flock -n -s /srv/cache/.sy-cache.lock true"
    sy-cache shared -- flock -n -s /srv/cache/.sy-cache.lock true
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

# Preserve the real OpenClaw L2 project scenario after its generic P0 fixture moved.
export SUBYARD_TEST_PROJECT_ENGINE="$YARD_BIN"
cat > "$STATE/project-yard" <<'PROJECT_YARD'
#!/usr/bin/env bash
set -euo pipefail
if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
  && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
  printf -v command '%q ' "$SUBYARD_TEST_PROJECT_ENGINE" "$@"
  exec sg incus-admin -c "exec $command"
fi
exec "$SUBYARD_TEST_PROJECT_ENGINE" "$@"
PROJECT_YARD
chmod 0700 "$STATE/project-yard"
printf 'openclaw_e2e_stage=projects\n'
PROJECT_CONTRACT_BIN="$STATE/project-yard" PROJECT_CONTRACT_YARD="$YARD_NAME" \
  PROJECT_CONTRACT_UID="$(id -u dev)" owner_project_contract openclaw

# shellcheck source=config/profiles/openclaw/tests/e2e/project-isolation.sh
. "$ROOT/config/profiles/openclaw/tests/e2e/project-isolation.sh"
printf 'openclaw_e2e_stage=parallel-isolation\n'
openclaw_project_isolation "$instance" "$project"

# Exercise staging container bring-up and teardown with a synthetic denylist fingerprint.
# Up leaves the gateway stopped; it does not need source code, bot credentials, or exclusive-key consent.
staging_status="$STATE/staging-status.out"
qa_status="$STATE/qa-status.out"
yard staging status > "$staging_status"
yard qa-pool status > "$qa_status"
grep -Fq "(no runner)" "$staging_status" || die 'fresh yard unexpectedly has a staging runner'
grep -Fq "(no broker)" "$qa_status" || die 'fresh yard unexpectedly has a QA broker'
install -d -m 0700 "$SUBYARD_CONFIG_HOME/overrides/host/staging"
printf "GATEWAY_CMD='exec sleep infinity'\nSOURCE_BIND=/srv/staging-fixture\nIMAGE_DOCKERFILE=Dockerfile\nIMAGE_TAG=subyard-staging-fixture\n" \
  > "$SUBYARD_CONFIG_HOME/overrides/host/staging/canonical.conf"
chmod 0600 "$SUBYARD_CONFIG_HOME/overrides/host/staging/canonical.conf"
yard shell --root -- install -d -m 0700 -o dev -g dev /srv/staging-fixture
yard shell -- sh -eu -c 'cat > /srv/staging-fixture/Dockerfile' <<'GATEWAY_IMAGE'
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends jq util-linux && rm -rf /var/lib/apt/lists/*
GATEWAY_IMAGE
yard staging up --yes > "$STATE/staging-up.out"
yard staging status > "$staging_status"
grep -Fq 'subyard-staging-canonical' "$staging_status" \
  || die 'staging status did not report its brought-up runner'
grep -Fq 'gateway: down' "$staging_status" \
  || die 'staging bring-up unexpectedly started the gateway'

# A compatible gateway command is an explicit input: no sibling app or account is needed.
write_staging_config() { # <staging-marker> <public-fixture-token>
  yard shell -- sh -eu -s -- "$1" "$2" <<'STAGING_CONFIG'
directory=/srv/staging/canonical/vasily/openclaw
install -d -m 0700 "$directory"
jq -n --argjson staging "$1" --arg token "$2" \
  '{_subyardStaging:$staging,channels:{telegram:{botToken:$token}}}' \
  > "$directory/openclaw.json"
chmod 0600 "$directory/openclaw.json"
STAGING_CONFIG
}
assert_no_staging_lease() {
  incus exec "$instance" --project "$project" -- test ! -e /srv/staging/_lease/bot.json \
    || die 'refused or stopped staging gateway retained a bot lease'
}
printf 'openclaw_e2e_stage=staging-guard\n'
write_staging_config false openclaw-e2e-synthetic-staging-token
if yard staging start --yes > "$STATE/staging-unmarked.out" 2>&1; then
  die 'staging start accepted an unmarked config'
fi
grep -Fq 'config not marked staging' "$STATE/staging-unmarked.out" \
  || die 'unmarked staging config failed for an unexpected reason'
assert_no_staging_lease
write_staging_config true openclaw-e2e-synthetic-denylist-token
if yard staging start --yes > "$STATE/staging-denied.out" 2>&1; then
  die 'staging start accepted a denied bot fingerprint'
fi
grep -Fq 'fingerprint matches a recorded PROD fingerprint' "$STATE/staging-denied.out" \
  || die 'denied bot fingerprint failed for an unexpected reason'
assert_no_staging_lease

printf 'openclaw_e2e_stage=staging-gateway\n'
write_staging_config true openclaw-e2e-synthetic-staging-token
enable_staging_capture
staging_step first-start "$STATE/staging-start.out" start --yes
staging_step running-status "$staging_status" status
grep -Fq 'gateway: running' "$staging_status" || die 'configured gateway did not start'
epoch="$(incus exec "$instance" --project "$project" -- \
  jq -er '.holder == "canonical" and (.epoch | type == "number")' /srv/staging/_lease/bot.json)"
[ "$epoch" = true ] || die 'running gateway did not own its bot lease'
epoch="$(incus exec "$instance" --project "$project" -- jq -er .epoch /srv/staging/_lease/bot.json)"
staging_step repeat-start "$STATE/staging-start-repeat.out" start --yes
[ "$(incus exec "$instance" --project "$project" -- jq -er .epoch /srv/staging/_lease/bot.json)" = "$epoch" ] \
  || die 'converged gateway start changed its lease epoch'
gateway_pid="$(incus exec "$instance" --project "$project" -- \
  docker exec subyard-staging-canonical cat /srv/staging/canonical/run/gateway.pid)"
[[ "$gateway_pid" =~ ^[1-9][0-9]*$ ]] || die 'running gateway has an invalid process ID'
staging_step stop "$STATE/staging-stop.out" stop --yes
staging_step stopped-status "$staging_status" status
grep -Fq 'gateway: down' "$staging_status" || die 'configured gateway did not stop'
incus exec "$instance" --project "$project" -- sh -eu -s -- "$gateway_pid" <<'VERIFY_GATEWAY_STOP'
[ "$(docker inspect -f '{{.State.Running}}' subyard-staging-canonical)" = true ]
docker exec subyard-staging-canonical sh -eu -c '
  if state="$(cat "/proc/$1/stat" 2>/dev/null)"; then
    state="${state##*) }"
    [ "${state%% *}" = Z ]
  else
    [ ! -e "/proc/$1/stat" ]
  fi
' sh "$1"
VERIFY_GATEWAY_STOP
assert_no_staging_lease
# Reconcile an old installation without replacing the flock inode.
incus exec "$instance" --project "$project" -- sh -eu -c '
  dir=/srv/staging/_lease
  printf "500\n" > "$dir/bot.epoch"
  chown 0:0 "$dir/bot.lock" "$dir/bot.epoch"
  chmod 0644 "$dir/bot.lock" "$dir/bot.epoch"
'
lease_inode="$(incus exec "$instance" --project "$project" -- stat -c '%d:%i' /srv/staging/_lease/bot.lock)"
staging_step existing-up "$STATE/staging-up-existing.out" up --yes
[ "$(incus exec "$instance" --project "$project" -- stat -c '%d:%i' /srv/staging/_lease/bot.lock)" = "$lease_inode" ] \
  || die 'staging up replaced the retained lease lock inode'
# An expired allocation is draining until its old container has been stopped.
incus exec "$instance" --project "$project" -- sh -eu -c '
  printf "%s\n" "{\"holder\":\"canonical\",\"kind\":\"ephemeral\",\"epoch\":500,\"expires\":0}" > /srv/staging/_lease/bot.json
  chown dev:dev /srv/staging/_lease/bot.json
  chmod 0644 /srv/staging/_lease/bot.json
'
if incus exec "$instance" --project "$project" -- su - dev -c 'sy-stage reserve --zone canonical' \
  > "$STATE/staging-expired-reserve.out" 2>&1; then
  die 'staging reserve replaced expired authority without a confirmed stop'
fi
grep -Fq 'stale: expired staging lease requires confirmed stop:' "$STATE/staging-expired-reserve.out" \
  || die 'expired staging authority was not reported stale'
incus exec "$instance" --project "$project" -- jq -e '.epoch == 500' /srv/staging/_lease/bot.json >/dev/null
staging_step expired-down "$STATE/staging-expired-down.out" down --yes
assert_no_staging_lease
staging_step expired-up "$STATE/staging-expired-up.out" up --yes
# Exercise the delivered guest entrypoint and its sibling payload as the yard user.
incus exec "$instance" --project "$project" -- su - dev -c 'sy-stage reserve --zone canonical' > "$STATE/staging-guest-reserve.out"
incus exec "$instance" --project "$project" -- jq -e \
  '.holder == "canonical" and .kind == "ephemeral" and .epoch == 501' /srv/staging/_lease/bot.json >/dev/null
incus exec "$instance" --project "$project" -- su - dev -c 'sy-stage release --zone canonical' > "$STATE/staging-guest-release.out"
assert_no_staging_lease
if yard qa-pool up --yes > "$STATE/qa-up.out" 2>&1; then
  die 'QA resource accepted bring-up without a broker source and generated secrets'
fi
grep -Fq 'BROKER_SRC unset' "$STATE/qa-up.out" \
  || die 'QA resource failed for a reason other than its missing broker source'
staging_step down "$STATE/staging-down.out" down --yes
staging_step down-status "$staging_status" status
grep -Fq 'exited' "$staging_status" || die 'staging down did not stop the runner'
staging_step destroy "$STATE/staging-destroy.out" destroy --yes
staging_step destroy-status "$staging_status" status
incus exec "$instance" --project "$project" -- test -d /srv/staging/canonical \
  || die 'staging destroy removed persistent zone data without --purge'
yard qa-pool status > "$qa_status"
grep -Fq "(no runner)" "$staging_status" || die 'staging destroy left its runner behind'
grep -Fq "(no broker)" "$qa_status" || die 'refused QA bring-up left a broker behind'

export PATH="$STAGING_ORIGINAL_PATH"
STAGING_CAPTURE_ACTIVE=0

# A converged public provision must perform no profile apply or cache/config rewrite.
before="$(incus exec "$instance" --project "$project" -- sh -eu -c \
  'stat -c "%u:%g:%i:%Y:%s" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/playwright /srv/cache/.sy-cache.lock /home/dev/.npmrc /home/dev/.config/pip/pip.conf')"
provision_repeat="$STATE/provision-repeat.out"
yard provision openclaw --yes > "$provision_repeat"
if grep -Fq 'provisioning openclaw' "$provision_repeat"; then
  die 'converged public provision ran the OpenClaw apply hook again'
fi
after="$(incus exec "$instance" --project "$project" -- sh -eu -c \
  'stat -c "%u:%g:%i:%Y:%s" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/playwright /srv/cache/.sy-cache.lock /home/dev/.npmrc /home/dev/.config/pip/pip.conf')"
[ "$before" = "$after" ] || die 'converged provision changed cache/config metadata'
check_guest_state

# This lane covers public provisioning, L2 projects, and the configured staging gateway. The external
# obligations in external-obligations.json remain separately incomplete.
printf 'ok: OpenClaw provisioning, L2 projects, staging guards, gateway lifecycle, and lease cleanup\n'
