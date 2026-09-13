#!/usr/bin/env bash
# QA/staging stale-state and lease protocol boundaries around central resource consent.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QA="$ROOT/config/profiles/openclaw/resources/qa-bot-broker/handler.sh"
STAGING="$ROOT/config/profiles/openclaw/resources/staging-gateway/handler.sh"
SY_STAGE="$ROOT/config/profiles/openclaw/resources/staging-gateway/sy-stage.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Execute the actual shell payload sent to docker exec, mapping only its mounted config path.
# The fixture credentials are synthetic and must not appear in output.
awk '/^RUN$/ { payload=0 } payload { print } /<<'"'"'RUN'"'"'$/ { payload=1 }' "$SY_STAGE" \
  | sed "s|^ef=/run/subyard/staging.env$|ef=$TMP/staging.env|" > "$TMP/model-run.sh"
[ -s "$TMP/model-run.sh" ] || fail 'missing staging model subprocess payload'
env SUBYARD_LIVE_MODEL=1 ANTHROPIC_API_KEY=ambient-fixture STAGING_MODEL_KEY=ambient-fixture \
  sh "$TMP/model-run.sh" sh -c \
  '[ -z "${SUBYARD_LIVE_MODEL:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ] && [ -z "${STAGING_MODEL_KEY:-}" ]' \
  > "$TMP/no-key.out" 2>&1 || fail 'no-key subprocess inherited a model credential or live signal'
printf 'STAGING_MODEL_KEY=staged-fixture\n' > "$TMP/staging.env"
sh "$TMP/model-run.sh" sh -c \
  '[ "$SUBYARD_LIVE_MODEL" = 1 ] && [ "$ANTHROPIC_API_KEY" = staged-fixture ] && [ "$STAGING_MODEL_KEY" = staged-fixture ]' \
  > "$TMP/key.out" 2>&1 || fail 'staged model credential did not reach the command'
set +e
sh "$TMP/model-run.sh" sh -c 'exit 23' > "$TMP/failed-command.out" 2>&1
model_status=$?
set -e
[ "$model_status" -eq 23 ] || fail 'staging model subprocess masked the command failure'
printf 'ANTHROPIC_API_KEY=fallback-fixture\n' > "$TMP/staging.env"
sh "$TMP/model-run.sh" sh -c \
  '[ "$SUBYARD_LIVE_MODEL" = 1 ] && [ "$ANTHROPIC_API_KEY" = fallback-fixture ] && [ "$STAGING_MODEL_KEY" = fallback-fixture ]' \
  > "$TMP/fallback-key.out" 2>&1 || fail 'mounted Anthropic fallback credential was lost'
if grep -Eq 'ambient-fixture|staged-fixture|fallback-fixture' \
  "$TMP/no-key.out" "$TMP/key.out" "$TMP/failed-command.out" "$TMP/fallback-key.out"; then
  fail 'staging model subprocess printed a credential'
fi

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home" SUBYARD_NO_AUDIT=1 PATH="$TMP/bin:$PATH"
export SUBYARD_CONFIG_HOST_DIR="$SUBYARD_CONFIG_HOME/overrides/host"
export SUBYARD_CONFIG_GENERATED_DIR="$SUBYARD_CONFIG_HOME/generated"
mkdir -p "$TMP/bin" "$SUBYARD_CONFIG_HOST_DIR" "$SUBYARD_CONFIG_GENERATED_DIR/qa-pool"

cat >"$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
log="$state_root/incus.log"
printf '%s\n' "$*" >>"$log"

case "${1:-}" in
  info) exit 0 ;;
  list) printf 'RUNNING\n'; exit 0 ;;
  file)
    if [ "${2:-}" = push ]; then
      case "${4:-}" in
        *prod-fingerprints)
          [ ! -e "$state_root/fail-fingerprint-push" ] || exit 1
          cp "$3" "$state_root/staged-prod-fingerprints"
          touch "$state_root/fingerprint-staged"
          ;;
        *) touch "$state_root/secret-staged" ;;
      esac
    fi
    exit 0
    ;;
  exec) ;;
  *) exit 0 ;;
esac

arguments=("$@")
separator=-1
for index in "${!arguments[@]}"; do
  if [ "${arguments[$index]}" = -- ]; then separator="$index"; break; fi
done
[ "$separator" -ge 0 ] || exit 2
command=("${arguments[@]:separator+1}")

case "${command[0]:-}" in
  docker)
    case "${command[1]:-}" in
      inspect)
        [ -e "$state_root/box-exists" ] || exit 1
        if [ "${command[2]:-}" = -f ]; then
          case "${command[3]:-}" in
            *Running*) [ -e "$state_root/box-running" ] && printf 'true\n' || printf 'false\n' ;;
            *Status*) [ -e "$state_root/box-running" ] && printf 'running\n' || printf 'exited\n' ;;
          esac
        fi
        ;;
      exec)
        joined=" ${command[*]} "
        case "$joined" in
          *' SUBYARD_PROD_FPS='*)
            prod_fps=""
            for argument in "${command[@]}"; do
              case "$argument" in SUBYARD_PROD_FPS=*) prod_fps="${argument#*=}" ;; esac
            done
            env \
              SUBYARD_PROD_FPS="$prod_fps" \
              VASILY_HOME="$state_root/staging/vasily" \
              SUBYARD_STAGING_DATA_ROOT="$state_root/staging" \
              OPENCLAW_CONFIG_PATH="$state_root/staging/vasily/openclaw/openclaw.json" \
              OPENCLAW_STATE_DIR="$state_root/staging/vasily/openclaw" \
              bash -s
            ;;
          *' [ -f "$1" ] && kill -0 '*) [ -e "$state_root/gateway-running" ] ;;
          *' setsid sh -c '*) touch "$state_root/gateway-running" ;;
          *' kill "$pid" '*) rm -f "$state_root/gateway-running" ;;
        esac
        ;;
      run) touch "$state_root/box-exists" "$state_root/box-running" ;;
      start) touch "$state_root/box-running" ;;
      *) : ;;
    esac
    ;;
  sh)
    case "${command[1]:-}" in
      -s)
        script="$(cat)"
        if [ "${command[3]:-}" = /srv/staging/_lease ]; then
          bash -s -- "$state_root/lease" "${command[@]:4}" <<<"$script"
        elif [ "${command[3]:-}" = /srv/env-secrets/qa-pool/secrets.env ]; then
          printf 'RESULT leased=1 distinct=1 exhausted=1\n'
        else
          bash -s -- "${command[@]:3}" <<<"$script"
        fi
        ;;
      -c)
        mapped=("${command[@]}")
        if [ "${mapped[4]:-}" = /srv/staging/_lease ]; then mapped[4]="$state_root/lease"; fi
        case "${mapped[4]:-}" in
          /srv/staging/canonical/*)
            mapped[4]="$state_root/staging/${mapped[4]##*/}"
            mkdir -p "$state_root/staging"
            ;;
        esac
        bash -c "${mapped[2]}" "${mapped[@]:3}"
        ;;
    esac
    ;;
  install) touch "$state_root/secret-staged" ;;
  id) [ "${command[1]:-}" = -g ] && printf '1001\n' ;;
  test)
    case "${command[1]:-} ${command[2]:-}" in
      '-r /srv/source/convex.json') exit 0 ;;
    esac
    ;;
esac
MOCK

cat >"$TMP/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod 0755 "$TMP/bin/incus" "$TMP/bin/sleep"
touch "$TMP/box-exists" "$TMP/box-running"
mkdir -p "$TMP/staging/vasily/openclaw"
staging_token='synthetic-staging-token'
staging_fp="$(printf '%s' "$staging_token" | sha256sum | cut -d' ' -f1)"
other_fp="$(printf '%s' 'synthetic-other-token' | sha256sum | cut -d' ' -f1)"
third_fp="$(printf '%s' 'synthetic-third-token' | sha256sum | cut -d' ' -f1)"
jq -n --arg token "$staging_token" \
  '{_subyardStaging:true,channels:{telegram:{botToken:$token}}}' \
  >"$TMP/staging/vasily/openclaw/openclaw.json"

cat >"$SUBYARD_CONFIG_GENERATED_DIR/qa-pool/secrets.env" <<EOF
OPENCLAW_QA_CONVEX_SECRET_MAINTAINER=test-only
OPENCLAW_QA_CONVEX_SECRET_CI=test-only
touch '$TMP/secret-sourced'
EOF
pool="$SUBYARD_CONFIG_GENERATED_DIR/qa-pool/pool.jsonl"

# A disappeared seed payload becomes an apply no-op before any credential access/staging.
rm -f "$pool" "$TMP/secret-sourced" "$TMP/secret-staged"
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=seed SUBYARD_OPERATION_ID=op-seed-empty \
  "$QA" seed >"$TMP/seed-empty.out"
[ ! -e "$TMP/secret-sourced" ] && [ ! -e "$TMP/secret-staged" ] \
  || fail 'QA seed no-op accessed or staged credentials'

# Smoke validates the already-seeded broker pool and does not depend on retaining host pool.jsonl.
SUBYARD_RESOURCE_MODE=prepare "$QA" smoke >"$TMP/smoke-retained.json"
grep -Fq '"action":"smoke","changed":true' "$TMP/smoke-retained.json" \
  || fail 'QA smoke rejected a retained broker without host pool.jsonl'
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=smoke SUBYARD_OPERATION_ID=op-smoke-retained \
  "$QA" smoke >"$TMP/smoke-retained.out"
[ -e "$TMP/secret-sourced" ] && [ -e "$TMP/secret-staged" ] \
  || fail 'authorized QA smoke did not access and stage credentials during apply'
rm -f "$TMP/secret-sourced" "$TMP/secret-staged"

# A stopped broker is stale/worse state for seed and smoke, again before secret handling.
printf '{"kind":"telegram","payload":{},"note":"fixture"}\n' >"$pool"
rm -f "$TMP/box-running" "$TMP/secret-sourced" "$TMP/secret-staged"
for action in seed smoke; do
  if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION="$action" \
    SUBYARD_OPERATION_ID="op-$action-stopped" "$QA" "$action" \
    >"$TMP/$action-stopped.out" 2>&1; then
    fail "QA $action accepted a stopped broker"
  fi
  [ ! -e "$TMP/secret-sourced" ] && [ ! -e "$TMP/secret-staged" ] \
    || fail "QA $action stopped-broker failure accessed or staged credentials"
done
touch "$TMP/box-running"

# The host staging guard must require one strict production fingerprint per non-comment line.
# Its real payload is executed by the transport fixture above; no canned verdict is used.
prod_fingerprints="$SUBYARD_CONFIG_HOST_DIR/prod-fingerprints"
assert_host_guard_rejects() { # <label>
  local label="$1"
  local mode
  for mode in prepare apply; do
    rm -rf "$TMP/lease" "$TMP/gateway-running"
    if SUBYARD_PROD_FPS="$other_fp" SUBYARD_RESOURCE_MODE="$mode" \
      SUBYARD_RESOURCE_ACTION=start SUBYARD_OPERATION_ID="guard-$label-$mode" \
      "$STAGING" start >"$TMP/host-guard-$label-$mode.out" 2>&1; then
      fail "host staging guard accepted $label production fingerprints in $mode"
    fi
    [ ! -e "$TMP/lease" ] && [ ! -e "$TMP/gateway-running" ] \
      || fail "host staging guard mutated lease or gateway state for $label fingerprints in $mode"
  done
}
rm -f "$prod_fingerprints"
assert_host_guard_rejects missing
if SUBYARD_RESOURCE_MODE=prepare "$STAGING" up >"$TMP/host-up-missing.out" 2>&1; then
  fail 'staging up prepare accepted a missing production fingerprint file'
fi
: >"$prod_fingerprints"
assert_host_guard_rejects empty
rm -f "$prod_fingerprints"; ln -s "$TMP/absent-fingerprint-target" "$prod_fingerprints"
assert_host_guard_rejects unreadable
rm -f "$prod_fingerprints"; printf 'not-a-sha256\n' >"$prod_fingerprints"
assert_host_guard_rejects malformed
printf '# comments only\n\n  # still no fingerprints\n' >"$prod_fingerprints"
assert_host_guard_rejects comments-only
printf '%s\nnot-a-sha256\n' "$other_fp" >"$prod_fingerprints"
assert_host_guard_rejects mixed-valid-malformed
if [ "$(id -u)" -ne 0 ]; then
  printf '%s\n' "$other_fp" >"$prod_fingerprints"
  chmod 000 "$prod_fingerprints"
  assert_host_guard_rejects unreadable-mode
  chmod 0600 "$prod_fingerprints"
fi
printf '%s\n' "$staging_fp" >"$prod_fingerprints"
assert_host_guard_rejects matching
printf '%s\n' "${staging_fp^^}" >"$prod_fingerprints"
assert_host_guard_rejects uppercase-matching
{
  printf '  # production fingerprints\n\n'
  printf '  %s  \n' "$(printf '%s' "$other_fp" | tr '[:lower:]' '[:upper:]')"
} >"$prod_fingerprints"
SUBYARD_RESOURCE_MODE=prepare \
  "$STAGING" start >"$TMP/host-guard-valid.json"
grep -Fq '"action":"start","changed":true' "$TMP/host-guard-valid.json" \
  || fail 'host staging guard rejected a valid uppercase fingerprint with comments and blank lines'
printf '%s\n' "$other_fp" >"$prod_fingerprints"

lease_dir="$TMP/lease"
lease="$lease_dir/bot.json"
lock="$lease_dir/bot.lock"
write_lease() { # <holder> <expiry-offset>
  local holder="$1" offset="$2" now
  now="$(date +%s)"
  mkdir -p "$lease_dir"
  printf '{"holder":"%s","kind":"canonical","epoch":7,"expires":%d}\n' \
    "$holder" "$((now + offset))" >"$lease"
  rm -f "$lock"
}
assert_lease_unchanged() { # <digest> <context>
  [ "$(sha256sum "$lease" | awk '{print $1}')" = "$1" ] \
    || fail "$2 changed lease bytes during prepare"
  [ ! -e "$lock" ] || fail "$2 created a lease lock during prepare"
}

# A live gateway is a no-op only with its exact, unexpired owned lease.
touch "$TMP/gateway-running"
write_lease foreign 3600
foreign_digest="$(sha256sum "$lease" | awk '{print $1}')"
if SUBYARD_RESOURCE_MODE=prepare "$STAGING" start >"$TMP/start-running-foreign.out" 2>&1; then
  fail 'running staging gateway accepted a foreign lease'
fi
assert_lease_unchanged "$foreign_digest" 'foreign running-gateway probe'

rm -f "$lease" "$lock"
if SUBYARD_RESOURCE_MODE=prepare "$STAGING" start >"$TMP/start-running-missing.out" 2>&1; then
  fail 'running staging gateway accepted a missing lease'
fi
[ ! -e "$lease" ] && [ ! -e "$lock" ] \
  || fail 'missing running-gateway probe created lease state'

write_lease canonical -1
expired_digest="$(sha256sum "$lease" | awk '{print $1}')"
if SUBYARD_RESOURCE_MODE=prepare "$STAGING" start >"$TMP/start-running-expired.out" 2>&1; then
  fail 'running staging gateway accepted an expired owned lease'
fi
assert_lease_unchanged "$expired_digest" 'expired running-gateway probe'

write_lease canonical 3600
owned_digest="$(sha256sum "$lease" | awk '{print $1}')"
SUBYARD_RESOURCE_MODE=prepare "$STAGING" start >"$TMP/start-running-owned.json"
grep -Fq '"action":"start","changed":false' "$TMP/start-running-owned.json" \
  || fail 'running gateway with exact owned lease was not a no-op'
assert_lease_unchanged "$owned_digest" 'owned running-gateway probe'

# Stop must reclaim an owned stale lease even when the gateway already stopped.
rm -f "$TMP/gateway-running"
rm -f "$prod_fingerprints"
SUBYARD_RESOURCE_MODE=prepare "$STAGING" status >"$TMP/status-without-fingerprints.json"
SUBYARD_RESOURCE_MODE=prepare "$STAGING" stop >"$TMP/stop-owned.json"
grep -Fq '"action":"stop","changed":true' "$TMP/stop-owned.json" \
  || fail 'stopped gateway with owned lease was not assessed as changed'
assert_lease_unchanged "$owned_digest" 'owned stop probe'
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=stop SUBYARD_OPERATION_ID=op-stop-owned \
  "$STAGING" stop >"$TMP/stop-owned.out"
[ ! -e "$lease" ] || fail 'staging stop left its owned scarce lease behind'
printf '%s\n' "$other_fp" >"$prod_fingerprints"

# A foreign live lease is impossible before consent and prepare cannot touch it.
write_lease foreign 3600
foreign_digest="$(sha256sum "$lease" | awk '{print $1}')"
if SUBYARD_RESOURCE_MODE=prepare "$STAGING" start >"$TMP/start-busy.out" 2>&1; then
  fail 'staging start accepted a busy foreign lease'
fi
assert_lease_unchanged "$foreign_digest" 'busy start probe'

# Missing lease is available without creation in prepare; acquisition happens only in apply.
rm -rf "$lease_dir"
SUBYARD_RESOURCE_MODE=prepare "$STAGING" start >"$TMP/start-available.json"
grep -Fq '"action":"start","changed":true' "$TMP/start-available.json" \
  || fail 'available staging start did not report a change'
[ ! -e "$lease" ] && [ ! -e "$lock" ] \
  || fail 'available staging prepare acquired or locked the lease'
[ ! -e "$lease_dir" ] || fail 'available staging prepare created the lease directory'
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=start SUBYARD_OPERATION_ID=op-start \
  "$STAGING" start >"$TMP/start.out"
jq -e '.holder == "canonical" and .kind == "canonical"' "$lease" >/dev/null \
  || fail 'staging apply did not acquire the exact zone lease'
[ -e "$lock" ] || fail 'staging apply did not create the lease lock'
[ -d "$lease_dir" ] || fail 'staging apply did not create the lease directory'
[ -e "$TMP/gateway-running" ] || fail 'staging apply did not launch the gateway'

# A fresh staging runner must use the account's actual primary group for shared caches.
# It can differ from DEV_UID; resetting it makes the OpenClaw provision check drift.
rm -f "$TMP/box-exists"
rm -f "$TMP/fingerprint-staged"
printf '# copy normalizes validated values\n%s\n' "${other_fp^^}" >"$prod_fingerprints"
touch "$TMP/fail-fingerprint-push"
: >"$TMP/incus.log"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up \
  SUBYARD_OPERATION_ID=op-up-fingerprint-fail "$STAGING" up \
  >"$TMP/up-fingerprint-fail.out" 2>&1
then
  fail 'staging up ignored a failed production fingerprint copy'
fi
[ ! -e "$TMP/fingerprint-staged" ] \
  || fail 'failed production fingerprint copy was reported as staged'
if grep -Eq -- '-- docker (start|run) ' "$TMP/incus.log"; then
  fail 'staging up started a runner after the production fingerprint copy failed'
fi
rm -f "$TMP/fail-fingerprint-push"
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-up-cache \
  "$STAGING" up >"$TMP/up-cache.out"
[ -e "$TMP/fingerprint-staged" ] \
  || fail 'staging up did not copy the validated production fingerprints'
printf '%s\n' "$other_fp" | cmp -s - "$TMP/staged-prod-fingerprints" \
  || fail 'staging up did not copy the normalized validated fingerprint snapshot'
printf '# refresh existing runner\n%s\n' "${third_fp^^}" >"$prod_fingerprints"
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-up-refresh \
  "$STAGING" up >"$TMP/up-refresh.out"
printf '%s\n' "$third_fp" | cmp -s - "$TMP/staged-prod-fingerprints" \
  || fail 'repeated staging up did not refresh the normalized fingerprint snapshot'
for cache in pnpm pip npm; do
  grep -Fq -- "-- install -d -o 1000 -g 1001 /srv/cache/$cache" "$TMP/incus.log" \
    || fail "staging did not preserve the primary group of the $cache cache"
done

# Exercise sy-stage itself, including its embedded guard, against marker-owned paths.
sy_root="$TMP/sy-stage"
sy_script="$sy_root/sy-stage"
mkdir -p "$sy_root/bin" "$sy_root/staging/diag/vasily/openclaw" \
  "$sy_root/staging/diag/run" "$sy_root/staging/diag/logs"
sed -e "s|^LEASE_DIR=\"/srv/staging/_lease\"$|LEASE_DIR=\"$sy_root/lease\"|" \
    -e "s|^STAGING_ROOT=\"/srv/staging\"$|STAGING_ROOT=\"$sy_root/staging\"|" \
    "$SY_STAGE" >"$sy_script"
chmod 0755 "$sy_script"
cp "$TMP/staging/vasily/openclaw/openclaw.json" \
  "$sy_root/staging/diag/vasily/openclaw/openclaw.json"
cat >"$sy_root/staging/diag/zone.env" <<EOF
CNAME='diag-box'
DATA_ROOT='$sy_root/staging/diag'
RUN_IMAGE='diag-image'
GATEWAY_CMD='true'
BUILD_CMD=''
BOT_LEASE_KEY='diag-bot'
LEASE_TTL='45'
CREDS_DEST=''
SOURCE_BIND='$ROOT'
GW_PID='$sy_root/staging/diag/run/gateway.pid'
HB_PID='$sy_root/staging/diag/run/heartbeat.pid'
YLOG='$sy_root/staging/diag/logs/gateway.log'
EOF
: >"$sy_root/staging/diag/run-args"
export SY_STAGE_FIXTURE_ROOT="$sy_root"
cat >"$sy_root/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
root="$SY_STAGE_FIXTURE_ROOT"
case "${1:-}" in
  inspect)
    [ -e "$root/box-exists" ] || exit 1
    [ "${2:-}" != -f ] || { printf 'running\n'; exit 0; }
    ;;
  exec)
    shift
    if [ "${1:-}" = -i ]; then
      shift
      prod_fps=""
      if [ "${1:-}" = -e ]; then prod_fps="${2#*=}"; shift 2; fi
      shift 3
      payload="$(cat)"
      env \
        SUBYARD_PROD_FPS="$prod_fps" \
        VASILY_HOME="$root/staging/diag/vasily" \
        SUBYARD_STAGING_DATA_ROOT="$root/staging/diag" \
        OPENCLAW_CONFIG_PATH="$root/staging/diag/vasily/openclaw/openclaw.json" \
        OPENCLAW_STATE_DIR="$root/staging/diag/vasily/openclaw" \
        bash -s <<<"$payload"
      exit
    fi
    if [ "${1:-}" = -d ]; then
      shift 2
      [ "${1:-}" != sh ] || shift 2
      script="${1:-}"
      case "$script" in
        *'setsid sh -c '*) printf '4242\n' >"$root/staging/diag/run/gateway.pid"; touch "$root/gateway-running" ;;
      esac
      exit 0
    fi
    shift
    case "${1:-}" in
      sh)
        script="${3:-}"
        case "$script" in
          *'kill "$pid"'*) rm -f "$root/gateway-running" "$root/staging/diag/run/gateway.pid" ;;
          *'kill -0 '*) [ -e "$root/gateway-running" ] ;;
          *) exit 0 ;;
        esac
        ;;
      cat) cat "$2" ;;
      *) exit 0 ;;
    esac
    ;;
  rm) rm -f "$root/box-exists" "$root/gateway-running" ;;
  run) touch "$root/box-exists" ;;
  *) exit 0 ;;
esac
MOCK
cat >"$sy_root/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod 0755 "$sy_root/bin/docker" "$sy_root/bin/sleep"
touch "$sy_root/box-exists"

sy_fingerprints="$sy_root/staging/diag/prod-fingerprints"
assert_sy_guard_rejects() { # <label>
  local label="$1"
  rm -rf "$sy_root/lease" "$sy_root/gateway-running"
  if SUBYARD_PROD_FPS="$other_fp" PATH="$sy_root/bin:$PATH" \
    "$sy_script" restart --zone diag >"$TMP/sy-guard-$label.out" 2>&1
  then
    fail "sy-stage accepted $label production fingerprints"
  fi
  [ ! -e "$sy_root/lease" ] && [ ! -e "$sy_root/gateway-running" ] \
    || fail "sy-stage mutated lease or gateway state for $label fingerprints"
}
rm -f "$sy_fingerprints"
assert_sy_guard_rejects missing
: >"$sy_fingerprints"
assert_sy_guard_rejects empty
rm -f "$sy_fingerprints"
ln -s "$sy_root/absent-fingerprint-target" "$sy_fingerprints"
assert_sy_guard_rejects unreadable
rm -f "$sy_fingerprints"
printf 'not-a-sha256\n' >"$sy_fingerprints"
assert_sy_guard_rejects malformed
printf '# comments only\n\n  # still no fingerprints\n' >"$sy_fingerprints"
assert_sy_guard_rejects comments-only
printf '%s\nnot-a-sha256\n' "$other_fp" >"$sy_fingerprints"
assert_sy_guard_rejects mixed-valid-malformed
if [ "$(id -u)" -ne 0 ]; then
  printf '%s\n' "$other_fp" >"$sy_fingerprints"
  chmod 000 "$sy_fingerprints"
  assert_sy_guard_rejects unreadable-mode
  chmod 0600 "$sy_fingerprints"
fi
printf '%s\n' "$staging_fp" >"$sy_fingerprints"
assert_sy_guard_rejects matching
printf '%s\n' "${staging_fp^^}" >"$sy_fingerprints"
assert_sy_guard_rejects uppercase-matching
{
  printf '# production fingerprints\n\n'
  printf '%s\n' "$(printf '%s' "$other_fp" | tr '[:lower:]' '[:upper:]')"
} >"$sy_fingerprints"
PATH="$sy_root/bin:$PATH" "$sy_script" restart --zone diag >"$TMP/sy-restart-1.out"
sy_lease_digest="$(sha256sum "$sy_root/lease/diag-bot.json" | awk '{print $1}')"
sy_gateway_pid="$(cat "$sy_root/staging/diag/run/gateway.pid")"
printf '%s\nnot-a-sha256\n' "$other_fp" >"$sy_fingerprints"
if PATH="$sy_root/bin:$PATH" "$sy_script" restart --zone diag \
  >"$TMP/sy-restart-existing-rejected.out" 2>&1
then
  fail 'sy-stage restart accepted malformed fingerprints while a gateway was running'
fi
[ "$(sha256sum "$sy_root/lease/diag-bot.json" | awk '{print $1}')" = "$sy_lease_digest" ] \
  && [ "$(cat "$sy_root/staging/diag/run/gateway.pid")" = "$sy_gateway_pid" ] \
  && [ -e "$sy_root/gateway-running" ] \
  || fail 'rejected sy-stage restart changed an existing lease or gateway'
printf '%s\n' "$other_fp" >"$sy_fingerprints"
jq -e '.holder == "diag" and .kind == "ephemeral"' "$sy_root/lease/diag-bot.json" >/dev/null \
  || fail 'rejected repeated sy-stage restart lost its exact lease'
PATH="$sy_root/bin:$PATH" "$sy_script" status --zone diag >"$TMP/sy-status.out"
grep -Fq 'gateway: running' "$TMP/sy-status.out" \
  || fail 'sy-stage status did not report the running fixture gateway'
PATH="$sy_root/bin:$PATH" "$sy_script" rebind --zone diag "$ROOT" >"$TMP/sy-rebind.out"
PATH="$sy_root/bin:$PATH" "$sy_script" restart --zone diag >"$TMP/sy-restart-rebound.out"
rm -f "$sy_fingerprints"
PATH="$sy_root/bin:$PATH" "$sy_script" status --zone diag >"$TMP/sy-status-no-fingerprints.out"
PATH="$sy_root/bin:$PATH" "$sy_script" stop --zone diag >"$TMP/sy-stop.out"
[ ! -e "$sy_root/lease/diag-bot.json" ] && [ ! -e "$sy_root/gateway-running" ] \
  || fail 'sy-stage stop left its lease or gateway marker behind'
PATH="$sy_root/bin:$PATH" "$sy_script" release --zone diag >"$TMP/sy-release.out"
PATH="$sy_root/bin:$PATH" "$sy_script" status --zone diag >"$TMP/sy-status-stopped.out"

printf 'ok: QA stale apply and staging fingerprint/lease boundaries are exact\n'
