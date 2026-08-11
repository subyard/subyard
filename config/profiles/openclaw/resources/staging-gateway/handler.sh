#!/usr/bin/env bash
# handler.sh — OpenClaw profile's shared staging gateway zones inside the yard,
# isolated from production. A staging zone is identified by a ZONE NAME (default `canonical`),
# NOT by a dev project — it is a shared service: dev-agents work in their own L2 boxes
# Project environments use the zone; they do not each run their own gateway with the one test
# bot. This is the O3 two-tier design (decisions-glossary "Staging design v2", 2026-06-27):
#   * canonical — persistent, built from `master`, baseline/smoke/demo, never accrues dirty state;
#   * ephemeral — a run of an agent's UNCOMMITTED worktree (test without committing). MVP = a LIVE BIND
#     of the worktree ('up --source'); reflink-snapshot isolation is deferred (P4). Serialized behind
#     canonical via a lease on the bot identity.
#
# Hard invariant — NOTHING here may touch production:
#   * a STAGING-only data root /srv/staging/<zone> (its own VASILY_HOME / state), never prod;
#   * STAGING-only credentials, mounted into the runner only (ro file / persistent creds vol),
#     never via -e, never under /srv/cache;
#   * a startup prod-fingerprint GUARD (ours; the project has no such check) that refuses to
#     start unless the config is marked staging AND the bot token's fingerprint is not on the
#     operator's host override prod denylist, plus state-root markers.
#   * the bot identity is the scarce resource: a flock+file LEASE (FIFO/TTL/epoch) admits one
#     poller at a time; handover is fence-by-lifecycle (stop the prior holder's gateway).
#
# Subcommands (a leading [zone] defaults to `canonical`):
#   up      [zone] [--rebuild] [--source PATH]   build/start the runner box (gateway stays down);
#                               --source PATH live-binds a worktree as /workspace (run uncommitted, no commit)
#   start   [zone]               prod-guard + acquire lease, then launch the gateway
#   stop    [zone]               stop the gateway + release the lease (keeps the box)
#   status  [zone]               box + gateway + lease + staging-fingerprint overview
#   logs    [zone] [-f]          tail the gateway log
#   shell   [zone]               interactive shell inside the runner box
#   down    [zone]               stop the box (keeps it + its staging data root)
#   destroy [zone] [--purge]     remove the box (--purge also wipes the staging data root)
#   list                         list staging-runner boxes in the yard
#
# Per-zone knobs come from overrides/host/staging; ledger consumers come from generated/staging.
# Operator-owned; no root. Docker here is the yard's nested daemon, never the host's.
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
. "$SCRIPT_DIR/lib-service.sh"   # profile shared-resource helpers: yexec, svc_require_yard_running

DEV_UID="${DEV_UID:-1000}"
PROFILES_DIR="$SCRIPT_DIR/../config/profiles"
: "${SUBYARD_CONFIG_HOST_DIR:?typed host config directory is required}"
: "${SUBYARD_CONFIG_GENERATED_DIR:?typed generated config directory is required}"
ZONES_DIR="$SUBYARD_CONFIG_HOST_DIR/staging"
STAGING_GENERATED_DIR="$SUBYARD_CONFIG_GENERATED_DIR/staging"
PROD_FP_FILE="$SUBYARD_CONFIG_HOST_DIR/prod-fingerprints"
LEASE_DIR="/srv/staging/_lease"                          # in the yard; one lease per bot identity

ydocker() { yexec docker "$@"; }
cname_for() { printf 'subyard-staging-%s' "$1"; }

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

require_resource_apply() { # <expected-local-action>
  local expected="$1"
  [ "${SUBYARD_RESOURCE_MODE:-}" = apply ] || die "resource apply mode is required"
  [ "${SUBYARD_RESOURCE_ACTION:-}" = "$expected" ] \
    || die "prepared resource action mismatch (expected '$expected')"
  [ -n "${SUBYARD_OPERATION_ID:-}" ] || die "resource apply operation ID is required"
}

sub="${1:-}"; shift || true
if [ -z "$sub" ]; then
  [ -z "${SUBYARD_RESOURCE_MODE:-}" ] || die "resource verb is required"
  _yard_help_and_exit
fi

# --- is-up: silent registry probe (yard status) — any zone with a live gateway pid? -----------
# Handled before zone resolution + the loud yard-running check: it must stay quiet and just
# return 0/1. "up" = any staging-runner box (any zone) has a live gateway pid (the box bind-mounts
# its data root at the same path, so the pid is /srv/staging/<zone>/run/gateway.pid).
if [ -z "${SUBYARD_RESOURCE_MODE:-}" ] && [ "$sub" = is-up ]; then
  incus info "$INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 || exit 1
  if yexec sh -c '
        for c in $(docker ps -q --filter "label=subyard.staging=1" 2>/dev/null); do
          z="$(docker inspect -f "{{ index .Config.Labels \"subyard.zone\" }}" "$c" 2>/dev/null)"
          [ -n "$z" ] || continue
          p="/srv/staging/$z/run/gateway.pid"
          docker exec "$c" sh -c "[ -f \"$p\" ] && kill -0 \"\$(cat \"$p\")\" 2>/dev/null" && exit 0
        done
        exit 1' 2>/dev/null
  then exit 0; else exit 1; fi
fi

if [ -z "${SUBYARD_RESOURCE_MODE:-}" ]; then
  case "$sub" in
    -h|--help|help) _yard_help_and_exit ;;
    *) die "typed resource dispatcher required for 'yard staging $sub'" ;;
  esac
fi

if [ "$sub" = list ]; then
  [ "$#" -eq 0 ] || die "'list' does not accept additional arguments"
  case "${SUBYARD_RESOURCE_MODE:-}" in
    prepare) emit_resource_assessment list false ;;
    apply)
      require_resource_apply list
      svc_require_yard_running
      echo "Staging-runner zones in the yard:"
      ydocker ps -a --filter "label=subyard.staging=1" \
        --format 'table {{.Label "subyard.zone"}}\t{{.Names}}\t{{.Status}}\t{{.Image}}' 2>/dev/null
      ;;
    *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
  esac
  exit 0
fi

# --- parse: [zone] plus verb-specific options ---------------------------------
zone="canonical"; rebuild=0; purge=0; follow=0; zone_set=0; src_override=""
while [ $# -gt 0 ]; do
  case "$1" in
    --rebuild) rebuild=1 ;;
    --source)  src_override="${2:-}"; [ -n "$src_override" ] || die "--source needs a yard path"; shift ;;
    --purge)   purge=1 ;;
    -f|--follow) follow=1 ;;
    -*)        die "unknown option '$1'" ;;
    *)         [ "$zone_set" = 1 ] && die "unexpected extra argument '$1'"; zone="$1"; zone_set=1 ;;
  esac
  shift
done
case "$zone" in *[!a-zA-Z0-9_-]*) die "zone name '$zone' must be [a-zA-Z0-9_-]" ;; esac
case "$sub" in
  up)
    [ "$purge" -eq 0 ] && [ "$follow" -eq 0 ] || die "'up' accepts only --rebuild and --source"
    ;;
  logs)
    [ "$rebuild" -eq 0 ] && [ "$purge" -eq 0 ] && [ -z "$src_override" ] \
      || die "'logs' accepts only -f or --follow"
    ;;
  destroy)
    [ "$rebuild" -eq 0 ] && [ "$follow" -eq 0 ] && [ -z "$src_override" ] \
      || die "'destroy' accepts only --purge"
    ;;
  start|stop|status|shell|down)
    [ "$rebuild" -eq 0 ] && [ "$purge" -eq 0 ] && [ "$follow" -eq 0 ] && [ -z "$src_override" ] \
      || die "'$sub' does not accept options"
    ;;
  *) die "unknown staging resource verb '$sub'" ;;
esac

svc_require_yard_running

# zone config (non-secret knobs) + defaults
PROFILE=openclaw
SOURCE_BIND=""                              # optional: a yard path to bind as the source tree
GATEWAY_CMD="scripts/vasily gateway run"
BUILD_CMD=""                                # optional: rebuild cmd run in the runner (cwd /workspace)
                                            #   before the gateway launches, so 'restart' picks up live edits
BOT_LEASE_KEY=bot                           # lease key = bot identity (shared across zones)
LEASE_TTL=45
CREDS_DEST=""                               # optional: path INSIDE the runner for a persistent creds store
                                            #   (a one-time manual provider login survives box recreate);
                                            #   backed by $dataRoot/creds. Empty => creds live under VASILY_HOME.
zconf="$ZONES_DIR/$zone.conf"
# shellcheck disable=SC1090
[ -r "$zconf" ] && . "$zconf"
[ -n "$src_override" ] && SOURCE_BIND="$src_override"   # --source overrides the zone-conf bind (live-bind a worktree)

profile="$PROFILE"
pf="$PROFILES_DIR/$profile/profile.conf"
[ -r "$pf" ] || die "zone '$zone' profile '$profile' has no profile.conf at $pf"

cname="$(cname_for "$zone")"
dataRoot="/srv/staging/$zone"
srcDir="$dataRoot/src"                       # /workspace inside the box
vasilyHome="$dataRoot/vasily"                # VASILY_HOME => staging-only state
ylog="$dataRoot/logs/gateway.log"
GW_PID="$dataRoot/run/gateway.pid"
HB_PID="$dataRoot/run/heartbeat.pid"
ysecret="/srv/env-secrets/staging-$zone/staging.env"
BOX_SECRET="/run/subyard/staging.env"

box_exists()  { ydocker inspect "$cname" >/dev/null 2>&1; }
box_running() { [ "$(ydocker inspect -f '{{.State.Running}}' "$cname" 2>/dev/null)" = true ]; }
require_box() { box_exists || die "no staging-runner for zone '$zone' — run: ${PROG:-yard} staging up $zone"; }
gateway_running() {
  ydocker exec "$cname" sh -c '[ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null' _ "$GW_PID" 2>/dev/null
}

# --- bot-identity lease (flock + file; FIFO/TTL/epoch; single host) -----------
# Runs in the yard as dev. mode=normal refuses a live foreign holder (PREEMPT if we may take
# it: ephemeral over canonical); mode=force overwrites (after the caller stopped the holder).
# Echoes "OK <epoch>" / "PREEMPT <holder>" / "BUSY <holder> <kind> <secs>".
lease_acquire() {
  local kind="$1" mode="${2:-normal}"
  yexec sh -s -- "$LEASE_DIR" "$BOT_LEASE_KEY" "$zone" "$kind" "$LEASE_TTL" "$mode" <<'LEASE'
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
  yexec sh -s -- "$LEASE_DIR" "$BOT_LEASE_KEY" "$zone" <<'LEASE'
set -eu
dir="$1"; key="$2"; me="$3"
st="$dir/$key.json"; lock="$dir/$key.lock"
[ -r "$st" ] || exit 0
exec 9>"$lock"; flock 9
[ "$(jq -r '.holder // ""' "$st")" = "$me" ] || exit 0
rm -f "$st"
LEASE
}
lease_show() { yexec sh -c '[ -r "$1/$2.json" ] && cat "$1/$2.json" || echo "{}"' _ "$LEASE_DIR" "$BOT_LEASE_KEY" 2>/dev/null; }

lease_owned() {
  yexec sh -c \
    '[ -r "$1/$2.json" ] && [ "$(jq -r ".holder // \"\"" "$1/$2.json")" = "$3" ]' \
    _ "$LEASE_DIR" "$BOT_LEASE_KEY" "$zone" 2>/dev/null
}

# Read-only lease probe. It never opens the lock or creates the lease directory.
lease_inspect() {
  yexec sh -s -- "$LEASE_DIR" "$BOT_LEASE_KEY" "$zone" <<'LEASE'
set -eu
st="$1/$2.json"; me="$3"
[ -r "$st" ] || { echo MISSING; exit 0; }
jq -e '
  type == "object" and
  (.holder | type == "string" and length > 0) and
  (.kind | type == "string" and length > 0) and
  (.expires | type == "number" and . >= 0)
' "$st" >/dev/null 2>&1 || { echo INVALID; exit 0; }
now=$(date +%s)
holder=$(jq -r '.holder' "$st")
kind=$(jq -r '.kind' "$st")
expires=$(jq -r '.expires' "$st")
if [ "$now" -ge "$expires" ]; then
  echo "EXPIRED $holder $kind"
elif [ "$holder" = "$me" ]; then
  echo "OWNED $kind $((expires-now))"
else
  echo "FOREIGN $holder $kind $((expires-now))"
fi
LEASE
}

validate_start_lease_available() {
  local lease_state
  lease_state="$(lease_inspect)"
  case "$lease_state" in
    MISSING|EXPIRED\ *) ;;
    OWNED\ canonical\ *) ;;
    OWNED\ *) die "zone '$zone' holds an incompatible bot lease: ${lease_state#OWNED }" ;;
    FOREIGN\ *) die "bot lease held: ${lease_state#FOREIGN } — another runner is polling; stop it or wait" ;;
    INVALID) die "bot lease state is invalid — repair it before staging start" ;;
    *) die "could not read bot lease availability" ;;
  esac
}

validate_running_gateway_lease() {
  local lease_state
  lease_state="$(lease_inspect)"
  case "$lease_state" in
    OWNED\ canonical\ *) return 0 ;;
    MISSING) die "gateway for zone '$zone' is running without its bot lease — run: ${PROG:-yard} staging stop $zone" ;;
    EXPIRED\ *) die "gateway for zone '$zone' is running with an expired bot lease — run: ${PROG:-yard} staging stop $zone" ;;
    FOREIGN\ *) die "gateway for zone '$zone' is running while the bot lease is foreign — run: ${PROG:-yard} staging stop $zone" ;;
    OWNED\ *) die "gateway for zone '$zone' is running with an incompatible bot lease — run: ${PROG:-yard} staging stop $zone" ;;
    INVALID) die "gateway for zone '$zone' has invalid bot lease state — run: ${PROG:-yard} staging stop $zone" ;;
    *) die "could not validate the running gateway bot lease" ;;
  esac
}

validate_start_guard() {
  local prod_fps="" guard_out=""
  require_box
  box_running \
    || die "staging-runner zone '$zone' is stopped — run: ${PROG:-yard} staging up $zone"

  validate_start_lease_available

  # The authority check is read-only and must happen before central consent or lease acquisition.
  "$SUBYARD_ROOT/bin/yard" keys check-exclusive "$zone" >/dev/null
  [ -r "$PROD_FP_FILE" ] \
    && prod_fps="$(grep -vE '^\s*(#|$)' "$PROD_FP_FILE" 2>/dev/null | tr -s '[:space:]' '\n' || true)"
  guard_out="$(ydocker exec -i -e "SUBYARD_PROD_FPS=$prod_fps" "$cname" sh -s <<'GUARD'
set -eu
cfg="${OPENCLAW_CONFIG_PATH:-$VASILY_HOME/openclaw/openclaw.json}"
[ -r "$cfg" ] || { echo "FAIL no staging config at $cfg — paste it first (yard staging shell)"; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "FAIL jq absent in the runner — cannot validate config"; exit 1; }
marker=0
[ "$(jq -r '._subyardStaging // false' "$cfg" 2>/dev/null)" = true ] && marker=1
[ -r /run/subyard/staging.env ] && grep -qE '^SUBYARD_STAGING=1\b' /run/subyard/staging.env && marker=1
[ "$marker" = 1 ] || { echo "FAIL config not marked staging (\"_subyardStaging\": true, or SUBYARD_STAGING=1 in staging.env)"; exit 1; }
sroot="${OPENCLAW_STATE_DIR:-$VASILY_HOME/openclaw}"
case "$sroot" in
  "$SUBYARD_STAGING_DATA_ROOT"/*) : ;;
  *) echo "FAIL state dir $sroot is not under the staging data root $SUBYARD_STAGING_DATA_ROOT"; exit 1 ;;
esac
tf="$(jq -r '.channels.telegram.tokenFile // ""' "$cfg" 2>/dev/null || true)"
if [ -n "$tf" ] && [ -r "$tf" ]; then tok="$(cat "$tf")"; else tok="$(jq -r '.channels.telegram.botToken // ""' "$cfg" 2>/dev/null || true)"; fi
[ -n "$tok" ] || { echo "FAIL no telegram bot token in $cfg (channels.telegram.botToken/tokenFile)"; exit 1; }
fp="$(printf '%s' "$tok" | sha256sum | cut -d' ' -f1)"
for bad in ${SUBYARD_PROD_FPS:-}; do
  [ "$fp" = "$bad" ] && { echo "FAIL bot-token fingerprint matches a recorded PROD fingerprint — refusing"; exit 1; }
done
echo OK
GUARD
)" || true
  case "$guard_out" in
    OK) ;;
    FAIL\ *) die "prod-guard refused start: ${guard_out#FAIL }" ;;
    *) die "prod-guard produced no verdict — refusing (fail-closed)" ;;
  esac
}

destroy_action() {
  if [ "$purge" -eq 1 ]; then printf 'destroy-purge\n'; else printf 'destroy\n'; fi
}

destroy_target_exists() {
  box_exists && return 0
  lease_owned && return 0
  yexec test -e "$(dirname "$ysecret")" >/dev/null 2>&1 && return 0
  [ "$purge" -eq 1 ] && yexec test -e "$dataRoot" >/dev/null 2>&1 && return 0
  return 1
}

prepare_resource() {
  local action changed=false
  case "$sub" in
    up)
      # shellcheck disable=SC1090
      . "$pf"
      : "${BASE_IMAGE:?profile $profile has no BASE_IMAGE}"
      [ -z "$SOURCE_BIND" ] || yexec test -d "$SOURCE_BIND" \
        || die "SOURCE_BIND is not a directory in the yard"
      emit_resource_assessment up true \
        "converge the isolated staging-runner box while leaving its gateway stopped"
      ;;
    start)
      if gateway_running; then
        validate_running_gateway_lease
        emit_resource_assessment start false
      else
        validate_start_guard
        emit_resource_assessment start true \
          "acquire the staging bot lease and launch its gateway heartbeat"
      fi
      ;;
    stop)
      if { box_exists && gateway_running; } || lease_owned; then
        emit_resource_assessment stop true "stop the staging gateway and release its bot lease"
      else
        emit_resource_assessment stop false
      fi
      ;;
    status) emit_resource_assessment status false ;;
    logs)
      require_box
      emit_resource_assessment logs false
      ;;
    shell)
      require_box
      box_running && gateway_running \
        || die "staging gateway for zone '$zone' is not running — run: ${PROG:-yard} staging start $zone"
      emit_resource_assessment shell false
      ;;
    down)
      if box_running || lease_owned; then
        emit_resource_assessment down true \
          "stop the staging-runner box and release its bot lease while preserving staging data"
      else
        emit_resource_assessment down false
      fi
      ;;
    destroy)
      action="$(destroy_action)"
      if destroy_target_exists; then changed=true; fi
      if [ "$changed" = false ]; then
        emit_resource_assessment "$action" false
      elif [ "$action" = destroy-purge ]; then
        emit_resource_assessment "$action" true \
          "remove the staging-runner box, staged credentials and bot lease" \
          "irreversibly delete the persistent staging data root"
      else
        emit_resource_assessment "$action" true \
          "remove the staging-runner box, staged credentials and bot lease while preserving staging data"
      fi
      ;;
    *) die "unknown staging resource verb '$sub'" ;;
  esac
}

case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    prepare_resource
    exit 0
    ;;
  apply)
    case "$sub" in
      destroy) require_resource_apply "$(destroy_action)" ;;
      *) require_resource_apply "$sub" ;;
    esac
    ;;
  *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
esac

case "$sub" in
  up)
    # shellcheck disable=SC1090
    . "$pf"
    : "${BASE_IMAGE:?profile $profile has no BASE_IMAGE}"
    df="${IMAGE_DOCKERFILE:-}"; run_image="$BASE_IMAGE"; ctx=""
    if [ -n "$df" ]; then ctx="${IMAGE_CONTEXT:-$(dirname "$df")}"; run_image="${IMAGE_TAG:-subyard-staging-$zone}"; fi

    sf="$STAGING_GENERATED_DIR/$zone.env"; have_secrets=0
    [ -r "$sf" ] && have_secrets=1

    # live-bind worktree (if any) must exist in the yard (run an agent's uncommitted code)
    [ -z "$SOURCE_BIND" ] || yexec test -d "$SOURCE_BIND" \
      || die "SOURCE_BIND '$SOURCE_BIND' is not a directory in the yard — point it at an agent's workspace (e.g. /srv/workspaces/<id>)"

    if box_exists; then
      ydocker start "$cname" >/dev/null
      # keep the in-yard CLI fresh (zone.env/run-args were written on first up)
      incus file push "$RESOURCE_DIR/sy-stage.sh" "$INSTANCE_NAME/usr/local/bin/sy-stage" "${PROJ[@]}" --mode 0755 --uid 0 --gid 0 >/dev/null 2>&1 || true
      ok "staging-runner zone '$zone' already exists — started (gateway down; '${PROG:-yard} staging start $zone' or in-yard 'sy-stage restart --zone $zone')"
      exit 0
    fi

    # source tree: a bound yard path, else the zone's own src dir (operator populates from main)
    src_desc="$srcDir (populate from main: clone/sync into it)"
    [ -n "$SOURCE_BIND" ] && src_desc="$SOURCE_BIND (bound)"

    for d in "$dataRoot" "$dataRoot/logs" "$dataRoot/run" "$dataRoot/creds" "$vasilyHome" "$srcDir" "$LEASE_DIR"; do
      yexec install -d -o "$DEV_UID" -g "$DEV_UID" "$d"
    done

    if [ -n "$df" ]; then
      src_for_build="${SOURCE_BIND:-$srcDir}"
      yexec test -r "$src_for_build/$df" \
        || die "zone wants '$df' in the source tree, but it's missing under $src_for_build — populate the source first (clone main / set SOURCE_BIND)"
      if [ "$rebuild" = 1 ] || ! ydocker image inspect "$run_image" >/dev/null 2>&1; then
        info "building env image '$run_image' from $df (context $ctx) …"
        ydocker build -t "$run_image" -f "$src_for_build/$df" "$src_for_build/$ctx" || die "env image build failed"
      else
        ok "env image '$run_image' already built (use --rebuild to force)"
      fi
    fi

    if [ "$have_secrets" = 1 ]; then
      yexec install -d -m 0700 -o "$DEV_UID" -g "$DEV_UID" "$(dirname "$ysecret")"
      incus file push "$sf" "$INSTANCE_NAME$ysecret" "${PROJ[@]}" \
        --mode 0600 --uid "$DEV_UID" --gid "$DEV_UID" >/dev/null \
        || die "could not stage staging/$zone.env into the yard"
    fi

    src_mount="${SOURCE_BIND:-$srcDir}"
    # mid = the STABLE run spec (everything bar the swappable source bind, the name/hostname and
    # the image/cmd). Reused verbatim by the in-yard 'sy-stage rebind' (written to run-args below).
    mid=(--restart unless-stopped
         --label subyard.staging=1 --label "subyard.zone=$zone" --label "subyard.profile=$profile"
         -v "$dataRoot:$dataRoot"
         -v "$LEASE_DIR:$LEASE_DIR"
         -e "VASILY_HOME=$vasilyHome"
         -e "SUBYARD_STAGING_ZONE=$zone"
         -e "SUBYARD_STAGING_DATA_ROOT=$dataRoot")
    for c in ${CACHES:-}; do yexec install -d -o "$DEV_UID" -g "$DEV_UID" "$c"; mid+=(-v "$c:$c"); done
    [ "$have_secrets" = 1 ] && mid+=(-v "$ysecret:$BOX_SECRET:ro")
    # persistent creds store (a one-time manual provider login survives box recreate); backed by $dataRoot/creds
    [ -n "$CREDS_DEST" ] && mid+=(-v "$dataRoot/creds:$CREDS_DEST")
    while IFS= read -r k; do
      case "$k" in
        PROFILE_NAME|BASE_IMAGE|CACHES|DEVICES|OPTIONAL_FEATURES|IMAGE_DOCKERFILE|IMAGE_CONTEXT|IMAGE_TAG|\
        ENV_MOUNTS|YARD_MOUNTS|YARD_CAPS|YARD_DEVICES) continue ;;
      esac
      mid+=(-e "$k=${!k}")
    done < <(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$pf" | cut -d= -f1 | sort -u)

    info "starting staging-runner zone '$zone' …"
    ydocker run -d --name "$cname" --hostname "staging-${zone}" \
      -v "$src_mount:/workspace" -w /workspace \
      "${mid[@]}" "$run_image" sleep infinity >/dev/null || die "docker run failed in the yard"
    ok "staging-runner zone '$zone' up (profile $profile, image $run_image)"

    # --- in-yard self-serve control plane (sy-stage): let the agent reserve+run from the yard ----
    gw_cmd_eff="${STAGING_GATEWAY_CMD:-$GATEWAY_CMD}"
    # zone.env — the self-contained spec the in-yard 'sy-stage' consumes (no host config in the yard).
    yexec sh -c 'cat > "$1"' _ "$dataRoot/zone.env" <<ZENV
CNAME='$cname'
DATA_ROOT='$dataRoot'
RUN_IMAGE='$run_image'
GATEWAY_CMD='$gw_cmd_eff'
BUILD_CMD='$BUILD_CMD'
BOT_LEASE_KEY='$BOT_LEASE_KEY'
LEASE_TTL='$LEASE_TTL'
CREDS_DEST='$CREDS_DEST'
SOURCE_BIND='$src_mount'
GW_PID='$GW_PID'
HB_PID='$HB_PID'
YLOG='$ylog'
ZENV
    # run-args — the reusable mid spec for 'sy-stage rebind' (one arg per line, preserves spaces).
    printf '%s\n' "${mid[@]}" | yexec sh -c 'cat > "$1"' _ "$dataRoot/run-args"
    # prod-fingerprints — the in-yard prod-guard reads this (deny-by-default stays effective).
    [ -r "$PROD_FP_FILE" ] && incus file push "$PROD_FP_FILE" "$INSTANCE_NAME$dataRoot/prod-fingerprints" "${PROJ[@]}" --mode 0644 >/dev/null 2>&1 || true
    # install the in-yard CLI on the agent's PATH.
    if incus file push "$RESOURCE_DIR/sy-stage.sh" "$INSTANCE_NAME/usr/local/bin/sy-stage" "${PROJ[@]}" --mode 0755 --uid 0 --gid 0 >/dev/null 2>&1; then
      ok "in-yard self-serve ready: in the yard the agent runs 'sy-stage restart --zone $zone' (reserve/restart/rebind/stop/status/logs)"
    else
      warn "could not install sy-stage into the yard — in-yard self-serve unavailable (operator-only via 'yard staging')"
    fi
    cat <<MSG

Next:
  1. Source tree at /workspace <- $src_desc.
       live-bind an agent's uncommitted worktree: ${PROG:-yard} staging up $zone --source /srv/workspaces/<id>
       (or set SOURCE_BIND in the host staging override); else populate $srcDir from \`master\`.
  2. Paste STAGING creds into the runner (one-time):
       ${PROG:-yard} staging shell $zone
       # set channels.telegram.tokenFile/botToken in \$VASILY_HOME/openclaw/openclaw.json,
       # mark it staging ("_subyardStaging": true), log in the staging model provider (Codex) once.
       # To survive box recreate, set CREDS_DEST in $zone.conf to the runner's creds dir
       # (persisted at $dataRoot/creds); else the login lives in the box until 'destroy'.
  3. Record PROD bot fingerprint(s) so the guard refuses them:
       printf '%s' "<PROD_BOT_TOKEN>" | sha256sum   # hash only
       echo "<that-hash>" >> "$SUBYARD_CONFIG_HOST_DIR/prod-fingerprints"
  4. ${PROG:-yard} staging start $zone
MSG
    ;;

  start)
    require_box
    if gateway_running; then
      validate_running_gateway_lease
      ok "gateway already running for zone '$zone' with its owned bot lease"
      exit 0
    fi
    validate_start_guard

    # --- acquire the bot lease (canonical takes it too, so ephemeral can preempt) ---
    la="$(lease_acquire canonical normal)"
    case "$la" in
      OK\ *)   epoch="${la#OK }"; ok "lease acquired (epoch $epoch)";;
      BUSY\ *) die "bot lease held: ${la#BUSY } — another runner is polling; stop it or wait";;
      *)       die "could not acquire bot lease: $la";;
    esac

    gw_cmd="${STAGING_GATEWAY_CMD:-$GATEWAY_CMD}"
    ydocker exec -d "$cname" sh -c '
      cd /workspace || exit 1
      mkdir -p "$(dirname "$2")"
      setsid sh -c "$1" >>"$2" 2>&1 &
      echo $! >"$3"
    ' _ "$gw_cmd" "$ylog" "$GW_PID"
    sleep 1
    if ! gateway_running; then lease_release || true; die "gateway exited immediately — check: ${PROG:-yard} staging logs $zone"; fi
    # heartbeat sidecar — runs INSIDE the box (tied to box lifetime, not this CLI process); it
    # renews the lease (flock on the bind-mounted lease file) while the gateway pid is alive,
    # then releases. The lease dir is mounted at the same path, so the same inode is locked.
    ydocker exec -d "$cname" sh -c '
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
    ok "gateway started for zone '$zone' (pid $(ydocker exec "$cname" cat "$GW_PID" 2>/dev/null))"
    info "follow it: ${PROG:-yard} staging logs $zone -f"
    ;;

  stop)
    if ! box_exists || ! gateway_running; then
      if lease_owned; then
        lease_release >/dev/null 2>&1 || true
        ok "gateway not running for zone '$zone'; released its remaining bot lease"
      else
        ok "gateway not running for zone '$zone'"
      fi
      exit 0
    fi
    ydocker exec "$cname" sh -c '
      pid="$(cat "$1" 2>/dev/null)"; [ -n "$pid" ] || exit 0
      kill "$pid" 2>/dev/null || true
      for _ in 1 2 3 4 5; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
      kill -9 "$pid" 2>/dev/null || true
      rm -f "$1"
    ' _ "$GW_PID"
    ok "gateway stopped for zone '$zone'"
    lease_release >/dev/null 2>&1 || true
    ;;

  status)
    if ! box_exists; then echo "zone '$zone': (no runner) — ${PROG:-yard} staging up $zone"; exit 0; fi
    echo "zone:    $zone (profile $profile)"
    echo "box:     $cname ($(ydocker inspect -f '{{.State.Status}}' "$cname" 2>/dev/null))"
    echo "data:    $dataRoot (VASILY_HOME=$vasilyHome)"
    if gateway_running; then echo "gateway: running (pid $(ydocker exec "$cname" cat "$GW_PID" 2>/dev/null))"; else echo "gateway: down"; fi
    echo "lease:   $(lease_show)"
    ydocker exec "$cname" sh -c '
      cfg="${OPENCLAW_CONFIG_PATH:-$VASILY_HOME/openclaw/openclaw.json}"
      command -v jq >/dev/null 2>&1 || { echo "config:  (jq absent)"; exit 0; }
      [ -r "$cfg" ] || { echo "config:  (none at $cfg)"; exit 0; }
      tf="$(jq -r ".channels.telegram.tokenFile // \"\"" "$cfg" 2>/dev/null)"
      if [ -n "$tf" ] && [ -r "$tf" ]; then tok="$(cat "$tf")"; else tok="$(jq -r ".channels.telegram.botToken // \"\"" "$cfg" 2>/dev/null)"; fi
      mk="$(jq -r "._subyardStaging // false" "$cfg" 2>/dev/null)"
      if [ -n "$tok" ]; then fp="$(printf "%s" "$tok" | sha256sum | cut -d" " -f1); echo "config:  staging-marker=$mk bot-fp=${fp%${fp#????????}}…"; else echo "config:  staging-marker=$mk (no bot token set)"; fi
    ' 2>/dev/null || true
    ;;

  logs)
    require_box
    if [ "$follow" = 1 ]; then
      exec incus exec "$INSTANCE_NAME" "${PROJ[@]}" -t -- docker exec "$cname" tail -n 200 -f "$ylog"
    fi
    ydocker exec "$cname" sh -c '[ -r "$1" ] && tail -n 200 "$1" || echo "(no gateway log yet at $1)"' _ "$ylog"
    ;;

  shell)
    require_box
    box_running && gateway_running \
      || die "staging gateway for zone '$zone' is not running — run: ${PROG:-yard} staging start $zone"
    exec incus exec "$INSTANCE_NAME" "${PROJ[@]}" -t -- docker exec -it "$cname" bash
    ;;

  down)
    if ! box_running; then
      if lease_owned; then
        lease_release >/dev/null 2>&1 || true
        ok "staging-runner zone '$zone' was stopped; released its remaining bot lease"
      else
        ok "staging-runner zone '$zone' already stopped"
      fi
      exit 0
    fi
    gateway_running && warn "gateway still running — stopping the box stops it too"
    lease_release >/dev/null 2>&1 || true
    ydocker stop "$cname" >/dev/null && ok "staging-runner zone '$zone' stopped"
    ;;

  destroy)
    if ! destroy_target_exists; then
      ok "no staging-runner state to destroy for zone '$zone'"
      exit 0
    fi
    lease_release >/dev/null 2>&1 || true
    ydocker rm -f "$cname" >/dev/null 2>&1 \
      && ok "staging-runner zone '$zone' destroyed" \
      || ok "no staging-runner box to remove for zone '$zone'"
    yexec rm -rf "$(dirname "$ysecret")" 2>/dev/null || true
    # `if`, not `[ … ] && …`: this is the destroy) arm's last command and the case is the script's
    # final action, so under set -e a false guard (the default, no --purge) would exit 1 despite a
    # successful destroy. Mirrors the sibling qa-bot-broker handler's cmd_destroy.
    if [ "$purge" = 1 ]; then yexec rm -rf "$dataRoot" 2>/dev/null && ok "staging data root wiped"; fi
    ;;

  *)
    die "unknown subcommand '$sub' (expected: up | start | stop | status | logs | shell | down | destroy | list)"
    ;;
esac
