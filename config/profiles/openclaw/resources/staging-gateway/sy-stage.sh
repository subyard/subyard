#!/usr/bin/env bash
# In-yard staging control for an OpenClaw coding agent.
#
# Runs INSIDE the yard and drives the yard's own Docker directly (the agent in L1 is in the
# `docker` group). No host `yard` CLI, no path typing: the runner is already live-bound to the
# agent's workspace, so "test my current code" = rebuild + restart the gateway.
#
# Provisioning (operator, ONCE, on the HOST): `yard staging up <zone> --source <ws>` builds the
# runner, stages creds, and writes into the yard: /srv/staging/<zone>/zone.env (this script's
# spec), /srv/staging/<zone>/run-args (rebind spec) and /srv/staging/<zone>/prod-fingerprints.
# It also installs this script at /usr/local/bin/sy-stage. After that the agent self-serves:
#
#   sy-stage reserve [--zone Z]            acquire the bot lease (ephemeral; preempts canonical)
#   sy-stage restart [--zone Z]            prod-guard + lease + rebuild from the live tree + (re)launch gateway
#   sy-stage rebind  [--zone Z] [PATH]     recreate the runner bound to PATH (default: current git worktree)
#   sy-stage stop    [--zone Z]            stop the gateway + release the lease
#   sy-stage release [--zone Z]            release the lease only
#   sy-stage status  [--zone Z]
#   sy-stage logs    [--zone Z] [-f]
#   sy-stage test    [--zone Z] -- CMD...  run CMD in the runner (cwd /workspace) with the host-config
#                                          staging MODEL key injected for THIS subprocess only — the
#                                          simple "no broker" live model-move path. SUBYARD_LIVE_MODEL=1
#                                          when a key is present; unset when absent. CMD decides whether to skip.
#
# The bot identity is the scarce resource: one poller at a time via a flock+file lease. An agent
# run is `ephemeral` and PREEMPTS a `canonical` holder (fence-by-lifecycle: stop it first).
set -euo pipefail

LEASE_DIR="/srv/staging/_lease"
STAGING_ROOT="/srv/staging"

die()  { printf 'sy-stage: %s\n' "$*" >&2; exit 1; }
ok()   { printf 'sy-stage: %s\n' "$*"; }
info() { printf 'sy-stage: %s\n' "$*" >&2; }

# Self-documenting how-to (Slice 3.3): works without docker or a provisioned zone so an agent given
# only a prompt can discover the lane. Mirrors /etc/subyard/openclaw-l1.md.
print_help() {
  cat <<'H'
sy-stage — in-yard self-serve staging / live control for a coding agent (runs INSIDE the yard).

Usage: sy-stage <command> [--zone Z] [PATH | -f | -- CMD...]

Commands:
  reserve            acquire the bot lease (ephemeral; preempts a canonical holder)
  restart            prod-guard + lease + rebuild from your live tree + (re)launch the gateway
  rebind [PATH]      recreate the runner bound to PATH (default: current git worktree)
  stop               stop the gateway + release the lease
  release            release the lease only
  status             zone / runner / gateway / lease
  logs [-f]          tail the gateway log (-f to follow)
  test -- CMD...     run CMD in the runner (cwd /workspace); injects the host-config staging provider
                     key as ANTHROPIC_API_KEY for THAT subprocess only (also SUBYARD_LIVE_MODEL=1, a
                     Subyard convenience flag the project does not read). CMD runs even with no key;
                     use that flag to report a missing prerequisite before running live tests.
  help               this text

Typical flows:
  live model, no gateway:   see the model-only wrapper in /etc/subyard/openclaw-l1.md
  full gateway e2e:         sy-stage reserve -> sy-stage restart -> sy-stage logs -f -> sy-stage stop

Constraints you must understand:
  - The test-bot is SHARED: one poller at a time (lease). An ephemeral run preempts a canonical
    holder; if another ephemeral run holds it you queue or must stop it.
  - The tree is LIVE-BOUND: restart/rebuild runs your CURRENT edits and writes into your workspace
    (NOT a snapshot).
  - The real test-bot has REAL side effects in the test chat; prod is refused by the prod-guard.
  - Test commands need their own live switches and model/provider selection. The full test:live
    matrix includes gateway suites and may fail without credentials. Command failures are preserved.
  - Caches are shared (/srv/cache); don't fork them per checkout. See /etc/subyard/openclaw-l1.md.

A zone must be provisioned by the operator first: yard staging up <zone> --source <ws>
H
}

sub="${1:-}"; shift || true
case "$sub" in ""|help|-h|--help) print_help; exit 0 ;; esac

command -v docker >/dev/null 2>&1 \
  || die "no docker here — sy-stage runs INSIDE the yard, where the agent (dev) is in the docker group"

zone="${SY_STAGE_ZONE:-canonical}"; follow=0; path_arg=""; cmd_args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --zone)      zone="${2:?--zone needs a name}"; shift ;;
    -f|--follow) follow=1 ;;
    --)          shift; cmd_args=("$@"); break ;;
    -*)          die "unknown option '$1'" ;;
    *)           path_arg="$1" ;;
  esac
  shift
done
case "$zone" in *[!a-zA-Z0-9_-]*) die "zone name '$zone' must be [a-zA-Z0-9_-]" ;; esac

zenv="$STAGING_ROOT/$zone/zone.env"
[ -r "$zenv" ] || die "zone '$zone' not provisioned in this yard ($zenv missing) — operator must run 'yard staging up $zone --source <ws>' on the host first"
# shellcheck disable=SC1090
. "$zenv"   # CNAME DATA_ROOT RUN_IMAGE GATEWAY_CMD BUILD_CMD BOT_LEASE_KEY LEASE_TTL CREDS_DEST SOURCE_BIND GW_PID HB_PID YLOG
: "${CNAME:?zone.env missing CNAME}" "${BOT_LEASE_KEY:?}" "${LEASE_TTL:?}" "${GW_PID:?}" "${YLOG:?}"

box_exists()      { docker inspect "$CNAME" >/dev/null 2>&1; }
require_box()     { box_exists || die "no runner for zone '$zone' — operator: 'yard staging up $zone --source <ws>'"; }
gateway_running() { docker exec "$CNAME" sh -c '[ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null' _ "$GW_PID" 2>/dev/null; }

# --- bot-identity lease (flock+file; FIFO/TTL/epoch; single host) — same primitive as the host CLI
lease_acquire() {  # kind [mode] -> "OK <epoch>" | "PREEMPT <holder>" | "BUSY <holder> <kind> <secs>"
  local kind="$1" mode="${2:-normal}"
  sh -s -- "$LEASE_DIR" "$BOT_LEASE_KEY" "$zone" "$kind" "$LEASE_TTL" "$mode" <<'LEASE'
set -eu
dir="$1"; key="$2"; me="$3"; kind="$4"; ttl="$5"; mode="$6"
install -d -m 0755 "$dir"
lock="$dir/$key.lock"; st="$dir/$key.json"
exec 9>"$lock"; flock 9
now=$(date +%s)
holder=""; hk=""; epoch=0; exp=0
if [ -r "$st" ]; then
  holder=$(jq -r '.holder // ""' "$st"); hk=$(jq -r '.kind // ""' "$st")
  epoch=$(jq -r '.epoch // 0' "$st");    exp=$(jq -r '.expires // 0' "$st")
fi
if [ "$mode" != force ] && [ -n "$holder" ] && [ "$holder" != "$me" ] && [ "$now" -lt "$exp" ]; then
  if [ "$kind" = ephemeral ] && [ "$hk" = canonical ]; then echo "PREEMPT $holder"; exit 0; fi
  echo "BUSY $holder $hk $((exp-now))"; exit 0
fi
epoch=$((epoch+1))
printf '{"holder":"%s","kind":"%s","epoch":%d,"expires":%d}\n' "$me" "$kind" "$epoch" "$((now+ttl))" >"$st"
echo "OK $epoch"
LEASE
}
lease_release() {
  sh -s -- "$LEASE_DIR" "$BOT_LEASE_KEY" "$zone" <<'LEASE'
set -eu
dir="$1"; key="$2"; me="$3"
st="$dir/$key.json"; lock="$dir/$key.lock"
[ -r "$st" ] || exit 0
exec 9>"$lock"; flock 9
[ "$(jq -r '.holder // ""' "$st")" = "$me" ] || exit 0
rm -f "$st"
LEASE
}
lease_show() { [ -r "$LEASE_DIR/$BOT_LEASE_KEY.json" ] && cat "$LEASE_DIR/$BOT_LEASE_KEY.json" || echo "{}"; }

# Acquire the lease for an agent (ephemeral). Preempts a canonical holder by stopping its gateway.
reserve_lease() {
  local la holder
  la="$(lease_acquire ephemeral normal)"
  case "$la" in
    OK\ *)      ok "lease acquired (epoch ${la#OK })"; return 0 ;;
    PREEMPT\ *) holder="${la#PREEMPT }"
                info "preempting canonical holder '$holder' (fence-by-lifecycle: stopping its gateway)"
                docker exec "subyard-staging-$holder" sh -c '
                  p="$(cat "$1" 2>/dev/null)"; [ -n "$p" ] && kill "$p" 2>/dev/null || true; rm -f "$1"
                ' _ "$STAGING_ROOT/$holder/run/gateway.pid" 2>/dev/null || true
                la="$(lease_acquire ephemeral force)"
                case "$la" in OK\ *) ok "lease acquired after preempt (epoch ${la#OK })"; return 0 ;; esac
                die "could not acquire lease after preempt: $la" ;;
    BUSY\ *)    die "bot lease held: ${la#BUSY } — another ephemeral run is polling; wait or 'sy-stage stop' it" ;;
    *)          die "could not acquire bot lease: $la" ;;
  esac
}

# prod-guard (deny-by-default) + lease + (re)launch gateway. Mirrors the host 'staging start'.
launch_gateway() {
  require_box
  local prod_fps="" fpf="$STAGING_ROOT/$zone/prod-fingerprints"
  [ -r "$fpf" ] && prod_fps="$(grep -vE '^\s*(#|$)' "$fpf" 2>/dev/null | tr -s '[:space:]' '\n' || true)"
  local guard_out
  guard_out="$(docker exec -i -e "SUBYARD_PROD_FPS=$prod_fps" "$CNAME" sh -s <<'GUARD'
set -eu
cfg="${OPENCLAW_CONFIG_PATH:-$VASILY_HOME/openclaw/openclaw.json}"
[ -r "$cfg" ] || { echo "FAIL no staging config at $cfg — paste it first (operator: yard staging shell)"; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "FAIL jq absent in the runner"; exit 1; }
marker=0
[ "$(jq -r '._subyardStaging // false' "$cfg" 2>/dev/null)" = true ] && marker=1
[ -r /run/subyard/staging.env ] && grep -qE '^SUBYARD_STAGING=1\b' /run/subyard/staging.env && marker=1
[ "$marker" = 1 ] || { echo "FAIL config not marked staging (\"_subyardStaging\": true)"; exit 1; }
sroot="${OPENCLAW_STATE_DIR:-$VASILY_HOME/openclaw}"
case "$sroot" in
  "$SUBYARD_STAGING_DATA_ROOT"/*) : ;;
  *) echo "FAIL state dir $sroot is not under the staging data root $SUBYARD_STAGING_DATA_ROOT"; exit 1 ;;
esac
tf="$(jq -r '.channels.telegram.tokenFile // ""' "$cfg" 2>/dev/null || true)"
if [ -n "$tf" ] && [ -r "$tf" ]; then tok="$(cat "$tf")"; else tok="$(jq -r '.channels.telegram.botToken // ""' "$cfg" 2>/dev/null || true)"; fi
[ -n "$tok" ] || { echo "FAIL no telegram bot token in $cfg"; exit 1; }
fp="$(printf '%s' "$tok" | sha256sum | cut -d' ' -f1)"
for bad in ${SUBYARD_PROD_FPS:-}; do
  [ "$fp" = "$bad" ] && { echo "FAIL bot-token fingerprint matches a recorded PROD fingerprint — refusing"; exit 1; }
done
echo "OK staging marker + state-root ok; bot fp ${fp%${fp#????????}}… not on prod denylist"
GUARD
)" || true
  case "$guard_out" in
    OK\ *)   ok "prod-guard: ${guard_out#OK }" ;;
    FAIL\ *) die "prod-guard refused: ${guard_out#FAIL }" ;;
    *)       die "prod-guard produced no verdict — refusing (fail-closed): '${guard_out:-<empty>}'" ;;
  esac
  [ -n "$prod_fps" ] || info "WARN prod-fingerprints empty — guard passed on the staging marker alone"

  reserve_lease

  # rebuild from the live-bound tree so 'restart' picks up the agent's current edits (synchronous)
  if [ -n "${BUILD_CMD:-}" ]; then
    info "rebuilding from the live tree: $BUILD_CMD (output -> $YLOG)"
    docker exec "$CNAME" sh -c 'cd /workspace || exit 1; mkdir -p "$(dirname "$2")"; { echo "=== build $(date) ==="; sh -c "$1"; } >>"$2" 2>&1' \
      _ "$BUILD_CMD" "$YLOG" || { lease_release || true; die "build failed (BUILD_CMD) — sy-stage logs --zone $zone"; }
    ok "build ok"
  fi

  info "launching the gateway: $GATEWAY_CMD"
  docker exec -d "$CNAME" sh -c '
    cd /workspace || exit 1
    mkdir -p "$(dirname "$2")"
    setsid sh -c "$1" >>"$2" 2>&1 &
    echo $! >"$3"
  ' _ "$GATEWAY_CMD" "$YLOG" "$GW_PID"
  sleep 1
  gateway_running || { lease_release || true; die "gateway exited immediately — sy-stage logs --zone $zone"; }
  # heartbeat sidecar — renews the lease while the gateway pid lives, then releases (inside the box)
  docker exec -d "$CNAME" sh -c '
    lease="$1/$2.json"; lock="$1/$2.lock"; me="$3"; ttl="$4"; gwpid="$5"; hbpid="$6"
    echo $$ >"$hbpid"
    step=$((ttl/3)); [ "$step" -gt 0 ] || step=5
    while [ -f "$gwpid" ] && kill -0 "$(cat "$gwpid" 2>/dev/null)" 2>/dev/null; do
      ( exec 9>"$lock"; flock 9
        [ -r "$lease" ] && [ "$(jq -r ".holder//\"\"" "$lease")" = "$me" ] || exit 0
        now=$(date +%s); t="$lease.t.$$"
        jq --argjson e "$((now+ttl))" ".expires=\$e" "$lease" >"$t" && mv "$t" "$lease"
      ) 2>/dev/null || true
      sleep "$step"
    done
    ( exec 9>"$lock"; flock 9
      [ -r "$lease" ] && [ "$(jq -r ".holder//\"\"" "$lease")" = "$me" ] && rm -f "$lease"
    ) 2>/dev/null || true
    rm -f "$hbpid"
  ' _ "$LEASE_DIR" "$BOT_LEASE_KEY" "$zone" "$LEASE_TTL" "$GW_PID" "$HB_PID"
  ok "gateway running for zone '$zone' (pid $(docker exec "$CNAME" cat "$GW_PID" 2>/dev/null)) — follow: sy-stage logs --zone $zone -f"
}

stop_gateway() {
  require_box
  if gateway_running; then
    docker exec "$CNAME" sh -c '
      pid="$(cat "$1" 2>/dev/null)"; [ -n "$pid" ] || exit 0
      kill "$pid" 2>/dev/null || true
      for _ in 1 2 3 4 5; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
      kill -9 "$pid" 2>/dev/null || true; rm -f "$1"
    ' _ "$GW_PID"
    ok "gateway stopped for zone '$zone'"
  else
    ok "gateway not running for zone '$zone'"
  fi
  lease_release >/dev/null 2>&1 || true
}

# Run a one-off test command in the runner with the host-config staging MODEL key injected for
# THIS subprocess only — never the runner's persistent env, never echoed. The key is resolved
# INSIDE the runner from the ro-mounted host-config (/run/subyard/staging.env: STAGING_MODEL_KEY,
# or ANTHROPIC_API_KEY), so it never crosses this CLI's process. Exports SUBYARD_LIVE_MODEL=1 when
# a key is present and clears it when absent. The command owns prerequisite skips and exit status.
# This is the "simple path, no broker" of the live-test lane: a live model turn from host-config.
run_test() {
  require_box
  local cmd=()
  if [ "${#cmd_args[@]}" -gt 0 ]; then
    cmd=("${cmd_args[@]}")
  elif [ -n "${TEST_LIVE_CMD:-}" ]; then
    cmd=(sh -c "$TEST_LIVE_CMD")
  else
    die "usage: sy-stage test [--zone Z] -- <cmd...>  — runs <cmd> in the runner (cwd /workspace) with the host-config staging provider key injected as ANTHROPIC_API_KEY for THIS run only (also SUBYARD_LIVE_MODEL=1, a Subyard convenience flag the project does not read). CMD runs even without a key; check SUBYARD_LIVE_MODEL for a prerequisite skip. See /etc/subyard/openclaw-l1.md for the model-only command. Set STAGING_MODEL_KEY in the zone's host-config staging.env."
  fi
  info "test command in zone '$zone' (staging provider key used when configured; cwd /workspace)"
  docker exec -i -w /workspace "$CNAME" sh -s -- "${cmd[@]}" <<'RUN'
set -eu
# Credentials for this invocation come only from the mounted staging config.
unset SUBYARD_LIVE_MODEL ANTHROPIC_API_KEY STAGING_MODEL_KEY
ef=/run/subyard/staging.env
key=""
[ -r "$ef" ] && key="$( . "$ef" 2>/dev/null; printf '%s' "${STAGING_MODEL_KEY:-${ANTHROPIC_API_KEY:-}}" )"
if [ -n "$key" ]; then
  # ANTHROPIC_API_KEY is the load-bearing inject (un-skips the project's key-gated live tests).
  # SUBYARD_LIVE_MODEL is a Subyard convenience flag only — the project does not read it.
  export SUBYARD_LIVE_MODEL=1 ANTHROPIC_API_KEY="$key" STAGING_MODEL_KEY="$key"
  echo "sy-stage: staging provider key injected as ANTHROPIC_API_KEY (this run only); SUBYARD_LIVE_MODEL=1" >&2
else
  echo "sy-stage: no staging provider key in host-config; running command without model credentials" >&2
fi
exec "$@"
RUN
}

case "$sub" in
  reserve) require_box; reserve_lease ;;
  restart) launch_gateway ;;
  stop)    stop_gateway ;;
  test)    run_test ;;
  release) lease_release && ok "lease released for zone '$zone'" ;;

  status)
    if ! box_exists; then echo "zone '$zone': (no runner) — operator: yard staging up $zone --source <ws>"; exit 0; fi
    echo "zone:    $zone"
    echo "box:     $CNAME ($(docker inspect -f '{{.State.Status}}' "$CNAME" 2>/dev/null))"
    echo "source:  ${SOURCE_BIND:-<zone src>} -> /workspace (live bind)"
    if gateway_running; then echo "gateway: running (pid $(docker exec "$CNAME" cat "$GW_PID" 2>/dev/null))"; else echo "gateway: down"; fi
    echo "lease:   $(lease_show)"
    ;;

  logs)
    require_box
    if [ "$follow" = 1 ]; then exec docker exec "$CNAME" tail -n 200 -f "$YLOG"; fi
    docker exec "$CNAME" sh -c '[ -r "$1" ] && tail -n 200 "$1" || echo "(no gateway log yet at $1)"' _ "$YLOG"
    ;;

  rebind)
    local_newsrc="${path_arg:-$(git -C "$PWD" rev-parse --show-toplevel 2>/dev/null || echo "$PWD")}"
    [ -d "$local_newsrc" ] || die "rebind source '$local_newsrc' is not a directory"
    spec="$STAGING_ROOT/$zone/run-args"
    [ -r "$spec" ] || die "no run-args spec for zone '$zone' — operator must re-run 'yard staging up $zone' once to enable in-yard rebind"
    mapfile -t midargs < "$spec"
    if gateway_running; then info "stopping current gateway before rebind"; stop_gateway; fi
    docker rm -f "$CNAME" >/dev/null 2>&1 || true
    docker run -d --name "$CNAME" --hostname "staging-${zone}" --restart unless-stopped \
      -v "$local_newsrc:/workspace" -w /workspace \
      "${midargs[@]}" "$RUN_IMAGE" sleep infinity >/dev/null \
      || die "docker run failed during rebind"
    # persist the new source in zone.env (replace the SOURCE_BIND line)
    tmp="$zenv.tmp.$$"
    grep -v '^SOURCE_BIND=' "$zenv" > "$tmp" 2>/dev/null || true
    printf "SOURCE_BIND='%s'\n" "$local_newsrc" >> "$tmp"; mv "$tmp" "$zenv"
    ok "zone '$zone' rebound to $local_newsrc — now: sy-stage restart --zone $zone"
    ;;

  *) die "unknown subcommand '$sub' (reserve|restart|rebind|stop|release|status|logs|test|help)" ;;
esac
