#!/usr/bin/env bash
# QA/staging stale-state and lease protocol boundaries around central resource consent.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QA="$ROOT/config/profiles/openclaw/resources/qa-bot-broker/handler.sh"
STAGING="$ROOT/config/profiles/openclaw/resources/staging-gateway/handler.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

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
    [ "${2:-}" = push ] && touch "$state_root/secret-staged"
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
          *' SUBYARD_PROD_FPS='*) printf 'OK\n' ;;
          *' [ -f "$1" ] && kill -0 '*) [ -e "$state_root/gateway-running" ] ;;
          *' setsid sh -c '*) touch "$state_root/gateway-running" ;;
          *' kill "$pid" '*) rm -f "$state_root/gateway-running" ;;
        esac
        ;;
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
        bash -c "${mapped[2]}" "${mapped[@]:3}"
        ;;
    esac
    ;;
  install) touch "$state_root/secret-staged" ;;
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
SUBYARD_RESOURCE_MODE=prepare "$STAGING" stop >"$TMP/stop-owned.json"
grep -Fq '"action":"stop","changed":true' "$TMP/stop-owned.json" \
  || fail 'stopped gateway with owned lease was not assessed as changed'
assert_lease_unchanged "$owned_digest" 'owned stop probe'
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=stop SUBYARD_OPERATION_ID=op-stop-owned \
  "$STAGING" stop >"$TMP/stop-owned.out"
[ ! -e "$lease" ] || fail 'staging stop left its owned scarce lease behind'

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

printf 'ok: QA stale apply and staging lease prepare/apply boundaries are exact\n'
