#!/usr/bin/env bash
# handler.sh — OpenClaw profile's in-yard QA credential broker and test-bot pool.
#
# Runs OpenClaw's `qa/convex-credential-broker` (a self-hosted Convex app) as ONE long-lived
# container on the YARD's Docker, bound to the yard loopback. It hands a leased test-bot (SUT +
# driver + group) to each `--credential-source convex` worker round-robin, so N dev-agents lease
# DIFFERENT bots with no Telegram 409, and an exhausted pool fails fast. This is the
# token-DISTRIBUTOR half of the live-test lane (the token-HIDER proxy is a separate axis).
#
# Host knobs come from overrides/host/qa-pool; ledger consumers come from generated/qa-pool.
#
# Subcommands:
#   up [--source PATH] [--redeploy]   start the backend + admin-key + `convex deploy` the broker
#                                     functions + set role secrets, then seed + expose. Idempotent.
#                                     --source PATH = the in-yard qa/convex-credential-broker dir
#                                     (overrides BROKER_SRC); --redeploy re-pushes functions/secrets.
#   seed                              (re)seed the generated pool (deduped by note).
#   expose                            (re)write the worker env (/srv/qa-pool/client.env: SITE url + CI
#                                     secret + insecure-http flag) an L1 dev-agent sources.
#   status                            backend state + pool summary (redacted) + endpoints.
#   logs [-f]                         tail the backend container log.
#   smoke                             self-test: lease every bot in the pool (distinct), prove the
#                                     next acquire is POOL_EXHAUSTED, then release all. Proves the DoD.
#   down                              stop the backend container (keeps data + pool).
#   destroy [--purge]                 remove the backend container (--purge also wipes /srv/qa-pool).
#
# Operator-owned; no host root. Docker here is the yard's nested daemon, never the host's.
set -euo pipefail
RESOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBYARD_ROOT="$(cd "$RESOURCE_DIR/../../../../.." && pwd)"
SCRIPT_DIR="$SUBYARD_ROOT/scripts"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"
# shellcheck source=scripts/lib-service.sh
. "$SCRIPT_DIR/lib-service.sh"   # yexec / svc_require_yard_running / PROJ / YARD_INSTANCE_NAME

DEV_UID="${DEV_UID:-1000}"
: "${SUBYARD_CONFIG_HOST_DIR:?typed host config directory is required}"
: "${SUBYARD_CONFIG_GENERATED_DIR:?typed generated config directory is required}"
CONF="$SUBYARD_CONFIG_HOST_DIR/qa-pool/broker.conf"
QA_GENERATED_DIR="$SUBYARD_CONFIG_GENERATED_DIR/qa-pool"
SECRETS="$QA_GENERATED_DIR/secrets.env"
POOL="$QA_GENERATED_DIR/pool.jsonl"

# In-yard layout (under the persistent /srv volume — survives instance rebuild, not --purge).
CNAME=subyard-qa-broker
DATA_ROOT=/srv/qa-pool
YDATA="$DATA_ROOT/data"                 # bind -> backend /convex/data (SQLite)
YADMIN="$DATA_ROOT/admin-key"           # generated admin key (convex-self-hosted|… prefix kept)
YINSTANCE="$DATA_ROOT/instance-secret"  # backend INSTANCE_SECRET (admin key derives from it)
YSRC="$DATA_ROOT/broker-src"            # copy of the broker project we deploy from
YCLIENT="$DATA_ROOT/client.env"         # worker env an L1 dev-agent sources
YDOC="$DATA_ROOT/qa-pool.md"            # agent-facing how-to
YDEPLOYED="$DATA_ROOT/.deployed"        # marker: functions+secrets pushed
YSECRETS=/srv/env-secrets/qa-pool/secrets.env
YPOOL=/srv/env-secrets/qa-pool/pool.jsonl

ydocker() { yexec docker "$@"; }

# --- knob defaults (broker.conf may override; env always wins via ${VAR:-}) --------
BACKEND_IMAGE="${BACKEND_IMAGE:-ghcr.io/get-convex/convex-backend:latest}"
DEPLOY_IMAGE="${DEPLOY_IMAGE:-node:22-bookworm-slim}"
CLOUD_PORT="${CLOUD_PORT:-3210}"
SITE_PORT="${SITE_PORT:-3211}"
KIND="${KIND:-telegram}"
OWNER_ID="${OWNER_ID:-yard-qa-pool-smoke}"
BROKER_SRC="${BROKER_SRC:-}"
# shellcheck disable=SC1090
[ -r "$CONF" ] && . "$CONF"

box_exists()  { ydocker inspect "$CNAME" >/dev/null 2>&1; }
box_running() { [ "$(ydocker inspect -f '{{.State.Running}}' "$CNAME" 2>/dev/null)" = true ]; }
require_box() { box_exists || die "no QA broker in the yard — run: ${PROG:-yard} qa-pool up"; }

# Validate the host-side secrets file has both role secrets (never print the values).
require_secrets() {
  [ -r "$SECRETS" ] || die "missing generated QA secrets at $SECRETS — import and materialize them with yard keys"
  local m c
  # shellcheck disable=SC1090 # operator-selected host secret file
  m="$( . "$SECRETS" >/dev/null 2>&1; printf '%s' "${OPENCLAW_QA_CONVEX_SECRET_MAINTAINER:-}")"
  # shellcheck disable=SC1090 # operator-selected host secret file
  c="$( . "$SECRETS" >/dev/null 2>&1; printf '%s' "${OPENCLAW_QA_CONVEX_SECRET_CI:-}")"
  [ -n "$m" ] || die "OPENCLAW_QA_CONVEX_SECRET_MAINTAINER is empty in $SECRETS (e.g. openssl rand -hex 32)"
  [ -n "$c" ] || die "OPENCLAW_QA_CONVEX_SECRET_CI is empty in $SECRETS"
}

# Push secrets.env into the yard (0600 dev) so in-yard ops read it without it crossing argv.
stage_secrets() {
  yexec install -d -m 0700 -o "$DEV_UID" -g "$DEV_UID" "$(dirname "$YSECRETS")"
  incus file push "$SECRETS" "$YARD_INSTANCE_NAME$YSECRETS" "${PROJ[@]}" \
    --mode 0600 --uid "$DEV_UID" --gid "$DEV_UID" >/dev/null \
    || die "could not stage secrets.env into the yard"
}

# ----------------------------------------------------------------------------------
# up — bring the broker online end to end (idempotent / resumable).
# ----------------------------------------------------------------------------------
cmd_up() {
  local src_override="" redeploy=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --source)   src_override="${2:-}"; [ -n "$src_override" ] || die "--source needs a yard path"; shift ;;
      --redeploy) redeploy=1 ;;
      -y|--yes)   ;;
      -*)         die "unknown option '$1'" ;;
      *)          die "unexpected argument '$1'" ;;
    esac
    shift
  done
  [ -n "$src_override" ] && BROKER_SRC="$src_override"

  svc_require_yard_running
  command -v incus >/dev/null 2>&1 || die "incus not found"
  require_secrets

  # The broker project must be present in the yard so we can `convex deploy` its functions.
  [ -n "$BROKER_SRC" ] || die "BROKER_SRC unset — point it at the in-yard qa/convex-credential-broker dir (broker.conf or --source), e.g. /srv/workspaces/<id>/vendor/openclaw/qa/convex-credential-broker"
  yexec test -r "$BROKER_SRC/convex.json" \
    || die "no convex.json under '$BROKER_SRC' in the yard — is it the qa/convex-credential-broker dir? (--source <yard-path>)"

  [ "${BACKEND_IMAGE##*:}" = latest ] \
    && warn "BACKEND_IMAGE is ':latest' — pin a commit-hash tag for a reproducible yard, compatible with convex 1.35.1 (see broker.conf)" \
    || true

  for d in "$DATA_ROOT" "$YDATA" "$YSRC" "$(dirname "$YSECRETS")"; do
    yexec install -d -o "$DEV_UID" -g "$DEV_UID" "$d"
  done
  stage_secrets

  # 1) instance secret — host override > persisted-in-yard > generate (and persist 0600).
  local host_instance
  # shellcheck disable=SC1090 # operator-selected host secret file
  host_instance="$( . "$SECRETS" >/dev/null 2>&1; printf '%s' "${CONVEX_INSTANCE_SECRET:-}")"
  if [ -n "$host_instance" ]; then
    printf '%s' "$host_instance" | yexec sh -c 'umask 077; cat > "$1"' _ "$YINSTANCE"
  elif ! yexec test -s "$YINSTANCE"; then
    yexec sh -c 'umask 077; { openssl rand -hex 32 2>/dev/null || tr -dc "a-f0-9" </dev/urandom | head -c 64; } > "$1"' _ "$YINSTANCE"
    info "generated a backend instance secret (persisted at $YINSTANCE)"
  fi
  yexec chown "$DEV_UID:$DEV_UID" "$YINSTANCE" 2>/dev/null || true

  # 2) backend container
  if box_exists; then
    box_running || ydocker start "$CNAME" >/dev/null
    ok "backend container '$CNAME' already present — started"
  else
    info "starting the Convex backend '$CNAME' …"
    ydocker run -d --name "$CNAME" --hostname qa-broker --restart unless-stopped \
      --label subyard.qa-broker=1 \
      -p "127.0.0.1:$CLOUD_PORT:3210" -p "127.0.0.1:$SITE_PORT:3211" \
      -v "$YDATA:/convex/data" \
      -e "YARD_INSTANCE_NAME=openclaw-qa-broker" \
      -e "INSTANCE_SECRET=$(yexec cat "$YINSTANCE")" \
      -e "CONVEX_CLOUD_ORIGIN=http://127.0.0.1:$CLOUD_PORT" \
      -e "CONVEX_SITE_ORIGIN=http://127.0.0.1:$SITE_PORT" \
      "$BACKEND_IMAGE" >/dev/null \
      || die "docker run failed in the yard (image $BACKEND_IMAGE) — is it pullable? try a pinned tag"
    ok "backend '$CNAME' up (image $BACKEND_IMAGE)"
  fi

  # 3) wait for health on the API port
  info "waiting for the backend API on yard 127.0.0.1:$CLOUD_PORT …"
  local _i ok_health=0
  for _i in $(seq 1 60); do
    if yexec sh -c "curl -fsS --max-time 3 http://127.0.0.1:$CLOUD_PORT/version >/dev/null 2>&1"; then ok_health=1; break; fi
    sleep 2
  done
  [ "$ok_health" = 1 ] || die "backend did not become healthy on 127.0.0.1:$CLOUD_PORT — check: ${PROG:-yard} qa-pool logs"

  # 4) admin key (reuse if already generated for this DB)
  if ! yexec test -s "$YADMIN"; then
    info "generating the self-hosted admin key …"
    local keyout
    keyout="$(ydocker exec "$CNAME" sh -c 'cd /convex 2>/dev/null || true; if [ -x ./generate_admin_key.sh ]; then ./generate_admin_key.sh; elif [ -x /convex/generate_admin_key.sh ]; then /convex/generate_admin_key.sh; else generate_admin_key.sh; fi' 2>/dev/null || true)"
    local key
    key="$(printf '%s\n' "$keyout" | grep -E 'convex-self-hosted\|' | tail -n1 || true)"
    [ -n "$key" ] || key="$(printf '%s\n' "$keyout" | grep -vE '^\s*$' | tail -n1 || true)"
    [ -n "$key" ] || die "could not read the admin key from generate_admin_key.sh — output was: ${keyout:-<empty>}"
    printf '%s' "$key" | yexec sh -c 'umask 077; cat > "$1"' _ "$YADMIN"
    yexec chown "$DEV_UID:$DEV_UID" "$YADMIN" 2>/dev/null || true
    ok "admin key generated (kept its convex-self-hosted| prefix)"
  fi

  # 5) copy the broker source into our data root (never mutate the agent's live tree)
  yexec sh -c 'rm -rf "$1"/* "$1"/.[!.]* 2>/dev/null; cp -a "$2/." "$1/"' _ "$YSRC" "$BROKER_SRC"
  yexec test -r "$YSRC/convex.json" || die "broker source copy is incomplete ($YSRC/convex.json missing)"

  # 6) deploy functions + register role secrets (skip on re-up unless --redeploy)
  if [ "$redeploy" = 1 ] || ! yexec test -f "$YDEPLOYED"; then
    info "deploying broker functions + secrets (npm install + convex deploy in a helper) …"
    deploy_functions || die "convex deploy failed — see the helper output above"
    yexec sh -c 'umask 077; : > "$1"' _ "$YDEPLOYED"
    ok "broker functions deployed + role secrets registered"
  else
    ok "functions already deployed (use --redeploy to push changes)"
  fi

  # 7) seed + expose
  cmd_seed
  cmd_expose

  cat <<MSG

QA broker ready. An L1 dev-agent runs a live Telegram/e2e test like:
  source $YCLIENT
  cd <their workspace> && pnpm openclaw qa telegram --credential-source convex
Self-test the pool:  ${PROG:-yard} qa-pool smoke
Status:              ${PROG:-yard} qa-pool status
MSG
}

# Run the convex CLI against the self-hosted backend from a throwaway Node helper. --network host
# so the helper reaches the loopback backend; secrets.env is mounted ro and read inside (env set
# uses the deployment env store — NOT container -e, which Convex functions can't read).
deploy_functions() {
  local admin
  admin="$(yexec cat "$YADMIN")"
  ydocker run --rm --network host \
    -v "$YSRC:/app" -v "$YSECRETS:/run/qa-secrets.env:ro" -w /app \
    -e "CONVEX_SELF_HOSTED_URL=http://127.0.0.1:$CLOUD_PORT" \
    -e "CONVEX_SELF_HOSTED_ADMIN_KEY=$admin" \
    "$DEPLOY_IMAGE" sh -ec '
      export HOME=/tmp CI=1
      npm install --no-audit --no-fund --loglevel=error
      cx=./node_modules/.bin/convex                       # the broker-pinned convex 1.35.1, not a floating npx
      "$cx" deploy -y
      . /run/qa-secrets.env
      "$cx" env set OPENCLAW_QA_CONVEX_SECRET_MAINTAINER "$OPENCLAW_QA_CONVEX_SECRET_MAINTAINER" >/dev/null
      "$cx" env set OPENCLAW_QA_CONVEX_SECRET_CI "$OPENCLAW_QA_CONVEX_SECRET_CI" >/dev/null
      echo "convex env:"; "$cx" env list | sed "s/=.*/=********/"
    '
}

# ----------------------------------------------------------------------------------
# seed — (re)seed the pool from pool.jsonl; idempotent (dedup by note).
# ----------------------------------------------------------------------------------
cmd_seed() {
  [ -r "$POOL" ] || { warn "no generated QA pool at $POOL — nothing to seed"; return 0; }
  require_box
  box_running || die "broker is not running — ${PROG:-yard} qa-pool up"
  require_secrets
  stage_secrets
  incus file push "$POOL" "$YARD_INSTANCE_NAME$YPOOL" "${PROJ[@]}" \
    --mode 0600 --uid "$DEV_UID" --gid "$DEV_UID" >/dev/null \
    || die "could not stage pool.jsonl into the yard"
  info "seeding the pool from pool.jsonl (deduped by note) …"
  local out
  out="$(yexec sh -s -- "$YSECRETS" "$YPOOL" "$SITE_PORT" <<'EOF'
set -eu
secrets="$1"; pool="$2"; port="$3"
. "$secrets"
maint="${OPENCLAW_QA_CONVEX_SECRET_MAINTAINER:-}"
base="http://127.0.0.1:$port/qa-credentials/v1"
existing="$(curl -sS --max-time 25 -X POST "$base/admin/list" \
  -H "authorization: Bearer $maint" -H 'content-type: application/json' \
  -d '{"status":"active","limit":500}' | jq -r '.credentials[]?.note // empty' 2>/dev/null || true)"
added=0; skipped=0; failed=0
while IFS= read -r line; do
  [ -n "$line" ] || continue
  printf '%s' "$line" | jq -e 'type=="object"' >/dev/null 2>&1 || continue
  if printf '%s' "$line" | jq -e '(keys|length)==1 and has("_comment")' >/dev/null 2>&1; then continue; fi
  kind="$(printf '%s' "$line" | jq -r '.kind // empty')"
  [ -n "$kind" ] || continue
  note="$(printf '%s' "$line" | jq -r '.note // empty')"
  if [ -z "$note" ]; then
    note="$kind:$(printf '%s' "$line" | jq -cS '.payload' | sha256sum | cut -c1-12)"
  fi
  if printf '%s\n' "$existing" | grep -qxF "$note"; then skipped=$((skipped+1)); continue; fi
  body="$(printf '%s' "$line" | jq -c --arg n "$note" '{kind:.kind, actorId:"yard-qa-pool", note:$n, payload:.payload}')"
  resp="$(curl -sS --max-time 25 -X POST "$base/admin/add" \
    -H "authorization: Bearer $maint" -H 'content-type: application/json' -d "$body")"
  if [ "$(printf '%s' "$resp" | jq -r '.status // "?"')" = ok ]; then
    added=$((added+1)); existing="$existing
$note"
  else
    failed=$((failed+1)); echo "  add failed (note=$note): $(printf '%s' "$resp" | jq -rc '.code // .status // .' 2>/dev/null)" >&2
  fi
done < "$pool"
echo "added=$added skipped=$skipped failed=$failed"
EOF
)" || die "seed failed (is the backend up + functions deployed?)"
  ok "pool seeded: $out"
  [ "${out##*failed=}" = 0 ] || warn "some entries failed to add — check pool.jsonl payloads (telegram needs numeric groupId + driverToken + sutToken)"
}

# ----------------------------------------------------------------------------------
# expose — write the worker env an L1 dev-agent sources (SITE url + CI secret + flag).
# ----------------------------------------------------------------------------------
cmd_expose() {
  require_box; require_secrets; stage_secrets
  yexec sh -s -- "$YSECRETS" "$YCLIENT" "$SITE_PORT" "$DEV_UID" <<'EOF'
set -eu
secrets="$1"; out="$2"; port="$3"; uid="$4"
. "$secrets"
umask 027
cat > "$out" <<ENV
# Worker env for the in-yard QA credential broker — source it before a --credential-source convex
# run. Written by 'yard qa-pool expose'. The CI secret + leased bot tokens live in the yard by
# design (token-DISTRIBUTOR; L1 access is best-effort). Do not commit.
export OPENCLAW_QA_CREDENTIAL_SOURCE=convex
export OPENCLAW_QA_CREDENTIAL_ROLE=ci
export OPENCLAW_QA_CONVEX_SITE_URL=http://127.0.0.1:$port
export OPENCLAW_QA_ALLOW_INSECURE_HTTP=1
export OPENCLAW_QA_CONVEX_SECRET_CI=${OPENCLAW_QA_CONVEX_SECRET_CI:-}
ENV
chown "$uid:$uid" "$out" 2>/dev/null || true
EOF
  write_doc
  ok "worker env written: $YCLIENT (source it in the yard before 'pnpm openclaw qa … --credential-source convex')"
}

# Agent-facing how-to (discovery): the in-yard self-serve doc.
write_doc() {
  yexec sh -s -- "$YDOC" "$YCLIENT" "$SITE_PORT" "$DEV_UID" <<'EOF'
set -eu
out="$1"; client="$2"; port="$3"; uid="$4"
umask 022
cat > "$out" <<DOC
# QA bot pool (in-yard credential broker)

A shared pool of STAGING Telegram test-bots lives behind a Convex broker on the yard loopback
(http://127.0.0.1:$port). Each run LEASES a bot (SUT + driver + group) round-robin, so parallel
agents get DIFFERENT bots (no Telegram 409). The lease auto-renews while your run holds it and is
released at the end; an exhausted pool fails fast (POOL_EXHAUSTED).

## Run a live Telegram / e2e test
  source $client
  cd <your workspace>
  pnpm openclaw qa telegram --credential-source convex          # bot-driver lane (kind=telegram)
  # or any qa-lab lane that honors --credential-source convex

## Constraints (you must understand these)
- The pool is SHARED and finite. If every bot is leased, acquire fails fast — retry later.
- Real test-bots: messages have real side effects in the test chat. Never point at prod.
- The lease has a TTL (default 20m, auto-heartbeat). A long run keeps its bot until it releases.
- You receive a real bot TOKEN for the lease (v1 broker has no app-encryption) — keep it out of
  logs/commits. Hiding tokens from the worker is a separate (deferred) proxy axis.
DOC
chown "$uid:$uid" "$out" 2>/dev/null || true
EOF
}

# ----------------------------------------------------------------------------------
# status / logs
# ----------------------------------------------------------------------------------
cmd_status() {
  svc_require_yard_running
  if ! box_exists; then echo "qa-pool: (no broker) — ${PROG:-yard} qa-pool up"; exit 0; fi
  echo "broker:   $CNAME ($(ydocker inspect -f '{{.State.Status}}' "$CNAME" 2>/dev/null))"
  echo "api:      http://127.0.0.1:$CLOUD_PORT (deploy/env)   site: http://127.0.0.1:$SITE_PORT (acquire/heartbeat/release)"
  echo "data:     $DATA_ROOT"
  if yexec test -r "$YCLIENT"; then echo "worker:   $YCLIENT (source it; role=ci)"; else echo "worker:   (not exposed) — ${PROG:-yard} qa-pool expose"; fi
  echo "pool:     credential and lease details withheld from read-only status; run smoke to validate"
}

cmd_logs() {
  require_box
  local follow=0; for a in "$@"; do case "$a" in -f|--follow) follow=1 ;; esac; done
  if [ "$follow" = 1 ]; then
    exec incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -t -- docker logs -n 200 -f "$CNAME"
  fi
  ydocker logs -n 200 "$CNAME" 2>&1 || true
}

# ----------------------------------------------------------------------------------
# smoke — DoD self-test: lease every bot (distinct), prove POOL_EXHAUSTED, release all.
# ----------------------------------------------------------------------------------
cmd_smoke() {
  require_box
  box_running || die "broker is not running — ${PROG:-yard} qa-pool up"
  require_secrets
  stage_secrets
  info "smoke-testing kind '$KIND' (acquire all → expect POOL_EXHAUSTED → release all) …"
  local out
  out="$(yexec sh -s -- "$YSECRETS" "$SITE_PORT" "$KIND" "$OWNER_ID" <<'EOF'
set -eu
secrets="$1"; port="$2"; kind="$3"; owner="$4"
. "$secrets"
ci="${OPENCLAW_QA_CONVEX_SECRET_CI:-}"
base="http://127.0.0.1:$port/qa-credentials/v1"
acq() { curl -sS --max-time 25 -X POST "$base/acquire" -H "authorization: Bearer $ci" \
  -H 'content-type: application/json' \
  -d "{\"kind\":\"$kind\",\"ownerId\":\"$owner\",\"actorRole\":\"ci\",\"leaseTtlMs\":60000,\"heartbeatIntervalMs\":30000}"; }
rel() { curl -sS --max-time 25 -X POST "$base/release" -H "authorization: Bearer $ci" \
  -H 'content-type: application/json' \
  -d "{\"kind\":\"$kind\",\"ownerId\":\"$owner\",\"actorRole\":\"ci\",\"credentialId\":\"$1\",\"leaseToken\":\"$2\"}" >/dev/null; }

ids=""; toks=""; n=0; distinct=1; exhausted=0
for _ in $(seq 1 50); do
  resp="$(acq)"
  st="$(printf '%s' "$resp" | jq -r '.status // "?"')"
  if [ "$st" = ok ]; then
    cid="$(printf '%s' "$resp" | jq -r '.credentialId')"
    tok="$(printf '%s' "$resp" | jq -r '.leaseToken')"
    haspay="$(printf '%s' "$resp" | jq -r 'if (.payload|type)=="object" then "yes" else "no" end')"
    case " $ids " in *" $cid "*) distinct=0 ;; esac
    ids="$ids $cid"; toks="$toks $cid=$tok"; n=$((n+1))
    echo "  acquired #$n: credentialId=$cid payload=$haspay"
  else
    code="$(printf '%s' "$resp" | jq -r '.code // .status')"
    echo "  next acquire -> $code (expected POOL_EXHAUSTED once the pool is drained)"
    [ "$code" = POOL_EXHAUSTED ] && exhausted=1
    break
  fi
done
# release everything we took
for pair in $toks; do cid="${pair%%=*}"; tok="${pair#*=}"; rel "$cid" "$tok"; done
echo "RESULT leased=$n distinct=$distinct exhausted=$exhausted"
EOF
)" || die "smoke failed (backend up? functions deployed? pool seeded?)"
  printf '%s\n' "$out" | sed '$d'
  local result; result="$(printf '%s\n' "$out" | tail -n1)"
  local leased distinct exhausted
  leased="$(printf '%s' "$result" | sed -n 's/.*leased=\([0-9]*\).*/\1/p')"
  distinct="$(printf '%s' "$result" | sed -n 's/.*distinct=\([0-9]*\).*/\1/p')"
  exhausted="$(printf '%s' "$result" | sed -n 's/.*exhausted=\([0-9]*\).*/\1/p')"
  [ "${leased:-0}" -ge 1 ] || die "smoke: leased 0 bots — seed the pool: ${PROG:-yard} qa-pool seed"
  [ "${distinct:-0}" = 1 ] || die "smoke: leased bots were NOT distinct — pool/round-robin broken"
  if [ "${exhausted:-0}" = 1 ]; then
    ok "smoke PASS: leased $leased distinct bot(s); next acquire fast-failed POOL_EXHAUSTED; all released"
  else
    warn "smoke: leased $leased distinct bot(s) and released them, but did not observe POOL_EXHAUSTED (pool > 50? or capped at the loop bound)"
  fi
}

# ----------------------------------------------------------------------------------
# down / destroy
# ----------------------------------------------------------------------------------
cmd_down() {
  if box_running; then
    ydocker stop "$CNAME" >/dev/null && ok "broker stopped (data + pool kept)"
  else
    ok "broker already stopped"
  fi
}

cmd_destroy() {
  local purge=0; for a in "$@"; do case "$a" in --purge) purge=1 ;; esac; done
  if ! qa_destroy_target_exists "$purge"; then
    ok "no QA broker state to destroy"
    return 0
  fi
  ydocker rm -f "$CNAME" >/dev/null 2>&1 && ok "broker container removed" || ok "no broker container to remove"
  yexec rm -rf "$(dirname "$YSECRETS")" 2>/dev/null || true
  if [ "$purge" = 1 ]; then yexec rm -rf "$DATA_ROOT" 2>/dev/null && ok "broker data root wiped"; fi
}

# ----------------------------------------------------------------------------------
emit_resource_assessment() { # <local-action> <true|false> [fixed consequence...]
  local action="$1" changed="$2" separator=""
  shift 2
  printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":%s,"consequences":[' \
    "$action" "$changed"
  local consequence
  for consequence in "$@"; do
    printf '%s"%s"' "$separator" "$consequence"
    separator=,
  done
  printf ']}\n'
}

require_no_resource_arguments() {
  local verb="$1"
  shift
  [ "$#" -eq 0 ] || die "'$verb' does not accept additional arguments"
}

validate_up_arguments() {
  local src_override=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --source)
        src_override="${2:-}"
        [ -n "$src_override" ] || die "--source needs a yard path"
        shift
        ;;
      --redeploy) ;;
      -*) die "unknown option '$1'" ;;
      *) die "unexpected argument '$1'" ;;
    esac
    shift
  done
  [ -z "$src_override" ] || BROKER_SRC="$src_override"
}

validate_logs_arguments() {
  local argument
  for argument in "$@"; do
    case "$argument" in
      -f|--follow) ;;
      *) die "logs does not accept argument '$argument'" ;;
    esac
  done
}

destroy_action_for_arguments() {
  local purge_requested=0 argument
  for argument in "$@"; do
    case "$argument" in
      --purge) purge_requested=1 ;;
      *) die "destroy does not accept argument '$argument'" ;;
    esac
  done
  if [ "$purge_requested" -eq 1 ]; then printf 'destroy-purge\n'; else printf 'destroy\n'; fi
}

qa_destroy_target_exists() { # <purge:0|1>
  box_exists && return 0
  yexec test -e "$(dirname "$YSECRETS")" >/dev/null 2>&1 && return 0
  [ "$1" -eq 1 ] && yexec test -e "$DATA_ROOT" >/dev/null 2>&1 && return 0
  return 1
}

prepare_resource() { # <public-verb> [validated args...]
  local verb="$1" action changed=false purge_flag=0
  shift
  case "$verb" in
    up)
      validate_up_arguments "$@"
      svc_require_yard_running
      command -v incus >/dev/null 2>&1 || die "incus not found"
      [ -n "$BROKER_SRC" ] \
        || die "BROKER_SRC unset — configure the in-yard QA broker source before running qa-pool up"
      yexec test -r "$BROKER_SRC/convex.json" \
        || die "the configured QA broker source has no readable convex.json"
      emit_resource_assessment up true \
        "converge the in-yard QA credential broker runtime" \
        "deploy broker functions and generated credentials" \
        "seed the generated bot pool and refresh its worker environment"
      ;;
    seed)
      require_no_resource_arguments seed "$@"
      svc_require_yard_running
      if [ ! -r "$POOL" ]; then
        emit_resource_assessment seed false
        return
      fi
      require_box
      box_running || die "QA broker is stopped — run: ${PROG:-yard} qa-pool up"
      emit_resource_assessment seed true "stage and merge the generated QA bot pool"
      ;;
    expose)
      require_no_resource_arguments expose "$@"
      svc_require_yard_running
      require_box
      emit_resource_assessment expose true "refresh the in-yard QA worker credential environment"
      ;;
    smoke)
      require_no_resource_arguments smoke "$@"
      svc_require_yard_running
      require_box
      box_running || die "QA broker is stopped — run: ${PROG:-yard} qa-pool up"
      emit_resource_assessment smoke true \
        "temporarily lease and release every generated QA bot to validate pool isolation"
      ;;
    down)
      require_no_resource_arguments down "$@"
      svc_require_yard_running
      if box_running; then
        emit_resource_assessment down true "stop the QA broker runtime while preserving its data"
      else
        emit_resource_assessment down false
      fi
      ;;
    destroy)
      action="$(destroy_action_for_arguments "$@")"
      svc_require_yard_running
      [ "$action" != destroy-purge ] || purge_flag=1
      qa_destroy_target_exists "$purge_flag" && changed=true
      if [ "$changed" = false ]; then
        emit_resource_assessment "$action" false
      elif [ "$action" = destroy-purge ]; then
        emit_resource_assessment "$action" true \
          "remove the QA broker runtime and staged credentials" \
          "irreversibly delete the persistent QA broker data root"
      else
        emit_resource_assessment "$action" true \
          "remove the QA broker runtime and staged credentials while preserving broker data"
      fi
      ;;
    status)
      require_no_resource_arguments status "$@"
      emit_resource_assessment status false
      ;;
    logs)
      validate_logs_arguments "$@"
      emit_resource_assessment logs false
      ;;
    *) die "unknown 'yard qa-pool' resource verb: '$verb'" ;;
  esac
}

require_resource_apply() { # <expected-local-action>
  local expected="$1"
  [ "${SUBYARD_RESOURCE_MODE:-}" = apply ] || die "resource apply mode is required"
  [ "${SUBYARD_RESOURCE_ACTION:-}" = "$expected" ] \
    || die "prepared resource action mismatch (expected '$expected')"
  [ -n "${SUBYARD_OPERATION_ID:-}" ] || die "resource apply operation ID is required"
}

sub="${1:-}"; [ $# -gt 0 ] && shift
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    [ -n "$sub" ] || die "resource verb is required"
    prepare_resource "$sub" "$@"
    ;;
  apply)
    case "$sub" in
      up)      validate_up_arguments "$@"; require_resource_apply up; cmd_up "$@" ;;
      seed)    require_no_resource_arguments seed "$@"; require_resource_apply seed; svc_require_yard_running; cmd_seed ;;
      expose)  require_no_resource_arguments expose "$@"; require_resource_apply expose; svc_require_yard_running; cmd_expose ;;
      status)  require_no_resource_arguments status "$@"; require_resource_apply status; cmd_status ;;
      logs)    validate_logs_arguments "$@"; require_resource_apply logs; svc_require_yard_running; cmd_logs "$@" ;;
      smoke)   require_no_resource_arguments smoke "$@"; require_resource_apply smoke; svc_require_yard_running; cmd_smoke ;;
      down)    require_no_resource_arguments down "$@"; require_resource_apply down; svc_require_yard_running; cmd_down ;;
      destroy)
        action="$(destroy_action_for_arguments "$@")"
        require_resource_apply "$action"
        svc_require_yard_running
        cmd_destroy "$@"
        ;;
      *) die "unknown 'yard qa-pool' apply verb: '$sub'" ;;
    esac
    ;;
  '')
    case "$sub" in
      is-up) box_running >/dev/null 2>&1 && exit 0 || exit 1 ;;
      ''|-h|--help) _yard_help_and_exit ;;
      *) die "typed resource dispatcher required for 'yard qa-pool $sub'" ;;
    esac
    ;;
  *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
esac
