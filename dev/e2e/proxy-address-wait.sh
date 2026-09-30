#!/usr/bin/env bash
# Real Incus/PID1 acceptance for generic local proxy-address boot readiness.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
die() { printf 'proxy-address-wait: %s\n' "$*" >&2; exit 2; }
case "${SUBYARD_E2E_VM:-}" in 1|2) ;; *) die 'run through dev/agent-e2e.sh on an allocated VM' ;; esac
RUN_ID="${SUBYARD_E2E_RUN_ID:-}"
[[ "$RUN_ID" =~ ^[0-9a-f]{8}$ ]] || die 'expected allocated eight-hex-digit run ID'
if [ "${SUBYARD_E2E_ADDRESS_WAIT_BOUNDED:-0}" != 1 ]; then
  exec timeout --foreground --kill-after=15 900 env SUBYARD_E2E_ADDRESS_WAIT_BOUNDED=1 bash "$0"
fi
for command in sudo incus ip jq systemctl systemd-run curl timeout; do
  command -v "$command" >/dev/null || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
MARKER="subyard-e2e-proxy-address-$RUN_ID"
PROJECT="$MARKER"
UNIT="$MARKER.service"
SLICE="subyard-e2e-address${RUN_ID}.slice"
CONTROL_UNIT="$MARKER-ip-control.service"
UNIT_PATH="/run/systemd/system/$UNIT"
BINARY_PATH="/usr/local/libexec/$MARKER-yard"
BINARY_HASH=
SLICE_PATH="/run/systemd/system/$SLICE"
INTERFACE="syaw$RUN_ID"
ADDRESS=192.0.2.123
STATE="$(mktemp -d /tmp/subyard-proxy-address.XXXXXXXX)"
chmod 0700 "$STATE"
printf '%s\n' "$MARKER" > "$STATE/.marker"
chmod 0600 "$STATE/.marker"
UNIT_ARMED=0 SLICE_ARMED=0 PROJECT_ARMED=0 INTERFACE_ARMED=0 BINARY_ARMED=0 CONTROL_ARMED=0
incus_local() { timeout --foreground --kill-after=5 60 sudo -n incus "$@"; }
owned_unit() {
  sudo -n test -f "$1" && sudo -n test ! -L "$1" \
    && [ "$(sudo -n head -n 1 "$1")" = "# $MARKER" ]
}
diagnostics() {
  sudo -n systemctl show "$UNIT" --property=ActiveState --property=SubState --property=Result --property=ExecMainStatus --property=NRestarts >&2 || true
  sudo -n journalctl -u "$UNIT" --no-pager -n 40 >&2 || true
  incus_local list --project "$PROJECT" --format csv -c ns >&2 || true
}
cleanup() {
  local rc=$? failed=0 name
  trap - EXIT INT TERM
  set +e
  if [ "$rc" != 0 ]; then diagnostics; fi
  if [ "$CONTROL_ARMED" = 1 ] \
    && [ "$(sudo -n systemctl show "$CONTROL_UNIT" --property=LoadState --value)" != not-found ]; then
    if [ "$(sudo -n systemctl show "$CONTROL_UNIT" --property=Description --value)" = "$MARKER IP accounting control" ]; then
      timeout --foreground 15 sudo -n systemctl stop "$CONTROL_UNIT" || failed=1
      sudo -n systemctl reset-failed "$CONTROL_UNIT" >/dev/null 2>&1 || true
    else failed=1; fi
  fi
  if [ "$UNIT_ARMED" = 1 ]; then
    if owned_unit "$UNIT_PATH"; then
      sudo -n systemctl stop "$UNIT" || failed=1
      sudo -n systemctl reset-failed "$UNIT" >/dev/null 2>&1 || true
      sudo -n rm -f -- "$UNIT_PATH" || failed=1
    else failed=1; fi
  fi
  if [ "$BINARY_ARMED" = 1 ]; then
    if sudo -n test -f "$BINARY_PATH" && sudo -n test ! -L "$BINARY_PATH" \
      && [ "$(sudo -n sha256sum "$BINARY_PATH" | awk '{print $1}')" = "$BINARY_HASH" ]; then
      sudo -n rm -f -- "$BINARY_PATH" || failed=1
    else failed=1; fi
  fi
  if [ "$PROJECT_ARMED" = 1 ]; then
    if [ "$(incus_local project get "$PROJECT" user.subyard.e2e 2>/dev/null)" = "$MARKER" ]; then
      for name in a-waiting z-ready; do
        if incus_local info "$name" --project "$PROJECT" >/dev/null 2>&1; then
          if [ "$(incus_local config get "$name" user.subyard.e2e --project "$PROJECT")" = "$MARKER" ]; then
            incus_local delete "$name" --force --project "$PROJECT" || failed=1
          else failed=1; fi
        fi
      done
      incus_local project delete "$PROJECT" || failed=1
    else failed=1; fi
  fi
  if [ "$INTERFACE_ARMED" = 1 ]; then
    if [ "$(ip -j link show dev "$INTERFACE" | jq -r '.[0].ifalias')" = "$MARKER" ]; then
      sudo -n ip link delete dev "$INTERFACE" || failed=1
    else failed=1; fi
  fi
  if [ "$SLICE_ARMED" = 1 ]; then
    if owned_unit "$SLICE_PATH"; then
      sudo -n systemctl stop "$SLICE" || failed=1
      sudo -n rm -f -- "$SLICE_PATH" || failed=1
    else failed=1; fi
  fi
  sudo -n systemctl daemon-reload || failed=1
  if [ -d "$STATE" ] && [ ! -L "$STATE" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; then
    find "$STATE" -depth -delete || failed=1
  else failed=1; fi
  [ "$failed" = 0 ] || rc=3
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Same product-owned baseline and cache marker as yard-network-policy.sh.
platform_root="$HOME/.cache/subyard-e2e-platform"
platform_marker="$platform_root/.subyard-e2e-platform-marker"
if [ -e "$platform_marker" ]; then
  [ -f "$platform_marker" ] && [ ! -L "$platform_marker" ] \
    && [ "$(cat "$platform_marker")" = subyard-e2e-platform-v1 ] || die 'unsafe platform marker'
fi
if ! incus_local info >/dev/null 2>&1 \
  || ! incus_local storage show default --project default >/dev/null 2>&1 \
  || ! incus_local network show incusbr0 --project default >/dev/null 2>&1; then
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$STATE/platform-bootstrap"
    export SUBYARD_USER
    SUBYARD_USER="$(id -un)"
    export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
    export SUBYARD_CONFIG_HOME="$STATE/platform-bootstrap-config" SUBYARD_HOME="$platform_root"
    export STORAGE_PATH="$platform_root/incus/incus/storage"
    export HOST_BASE="$STATE/platform-host-data" RESTRICTED_DISK_PATHS="$STATE/platform-host-data"
    set -a
    # shellcheck source=config/host.env
    . "$ROOT/config/host.env"
    set +a
    timeout --foreground --kill-after=10 300 bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
  )
fi
timeout --foreground 180 sudo -n incus admin waitready >/dev/null
incus_local storage show default --project default >/dev/null
incus_local network show incusbr0 --project default >/dev/null
if [ ! -e "$platform_marker" ]; then
  install -d -m 0711 "$platform_root"
  temporary="$(mktemp "$platform_root/.platform-marker.XXXXXX")"
  printf '%s\n' subyard-e2e-platform-v1 > "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$platform_marker"
fi
# Incus installer owns the NetworkManager guard; never waive it in this fixture.
# Fail closed if unrelated managed yards exist: _power-reconcile is host-wide.
incus_local list --all-projects --format json | jq -e \
  'all(.[]; (.expanded_config["user.subyard.managed"] // .config["user.subyard.managed"] // "false") != "true")' >/dev/null \
  || die 'unexpected managed yards on disposable host'
! incus_local project show "$PROJECT" >/dev/null 2>&1 || die 'fixture project already exists'
! ip link show dev "$INTERFACE" >/dev/null 2>&1 || die 'fixture interface already exists'
! ip -j address show | jq -e --arg address "$ADDRESS" 'any(.[].addr_info[]; .local == $address)' >/dev/null \
  || die 'synthetic address is already present'
for path in "$UNIT_PATH" "$SLICE_PATH" "$BINARY_PATH"; do
  sudo -n test ! -e "$path" && sudo -n test ! -L "$path" || die "runtime path already exists: $path"
done
[ "$(sudo -n systemctl show "$CONTROL_UNIT" --property=LoadState --value)" = not-found ] \
  || die 'IP accounting control unit already exists'
timeout --foreground --kill-after=10 180 bash "$ROOT/dev/build-engine.sh" >/dev/null
YARD_BIN="$ROOT/.build/yard"
# Execute a verified copy outside the PrivateTmp-hidden worktree and /run execution restrictions.
BINARY_HASH="$(sha256sum "$YARD_BIN" | awk '{print $1}')"
sudo -n install -D -m 0755 "$YARD_BIN" "$BINARY_PATH"
printf 'evidence: binary_sha256=%s\n' "$BINARY_HASH"
BINARY_ARMED=1
IMAGE=subyard-e2e-debian-13-cloud-container
if ! incus_local image info "$IMAGE" --project default >/dev/null 2>&1; then
  timeout --foreground --kill-after=10 300 sudo -n incus image copy images:debian/13/cloud local: \
    --alias "$IMAGE" --project default >/dev/null
fi
sudo -n ip link add "$INTERFACE" type dummy
INTERFACE_ARMED=1
sudo -n ip link set dev "$INTERFACE" alias "$MARKER"
sudo -n ip link set dev "$INTERFACE" up
incus_local project create "$PROJECT" -c features.images=false -c features.profiles=false \
  -c user.subyard.e2e="$MARKER" >/dev/null
PROJECT_ARMED=1
for name in a-waiting z-ready; do
  incus_local init "$IMAGE" "$name" --project "$PROJECT" --storage default \
    -c user.subyard.e2e="$MARKER" -c boot.autostart=false \
    -c user.subyard.managed=true -c user.subyard.initialized=true \
    -c user.subyard.desired_power=running -c user.subyard.name="$name" \
    -c user.subyard.bridge=incusbr0 >/dev/null
  if [ "$name" = a-waiting ]; then listen="$ADDRESS"; else listen=127.0.0.1; fi
  incus_local config device add "$name" address-test proxy --project "$PROJECT" \
    listen="tcp:$listen:38473" connect=tcp:127.0.0.1:22 bind=host nat=false >/dev/null
done
{
  printf '# %s\n[Slice]\nCPUAccounting=yes\nMemoryAccounting=yes\nIPAccounting=yes\n' "$MARKER"
} | sudo -n install -m 0644 /dev/stdin "$SLICE_PATH"
SLICE_ARMED=1
{
  printf '# %s\n' "$MARKER"
  sed "s|@SUBYARD_POWER_RECONCILER@|$BINARY_PATH|g" "$ROOT/config/systemd/subyard-power-reconcile.service.in"
  printf '\n[Service]\nSlice=%s\nCPUAccounting=yes\nMemoryAccounting=yes\nIPAccounting=yes\n' "$SLICE"
} | sudo -n install -m 0644 /dev/stdin "$UNIT_PATH"
UNIT_ARMED=1
sudo -n systemctl daemon-reload
sudo -n systemctl start --no-block "$UNIT"
state_is() { [ "$(incus_local list "$1" --project "$PROJECT" --format csv -c s)" = "$2" ]; }
wait_for() {
  local deadline=$((SECONDS + $1))
  shift
  while (( SECONDS < deadline )); do "$@" && return 0; sleep 1; done
  die 'timed out waiting for fixture transition'
}
has_waiting() {
  sudo -n journalctl -u "$UNIT" --no-pager -o cat | grep -Fq \
    "subyard-power: waiting-for-address $PROJECT/a-waiting: $ADDRESS"
}
expected_wait_state() {
  local snapshot
  snapshot="$(sudo -n systemctl show "$UNIT" --property=ActiveState --property=SubState \
    --property=Result --property=ExecMainStatus --property=MainPID --property=NRestarts)"
  grep -Fxq 'SubState=auto-restart' <<<"$snapshot" \
    && grep -Fxq 'Result=success' <<<"$snapshot" \
    && grep -Fxq 'ExecMainStatus=75' <<<"$snapshot" \
    && grep -Fxq 'MainPID=0' <<<"$snapshot"
}
wait_for 45 has_waiting
wait_for 45 state_is z-ready RUNNING
state_is a-waiting STOPPED || die 'missing-address yard started prematurely'
wait_for 35 expected_wait_state
printf 'evidence: waiting_for_address=%s/%s:%s independent_ready=RUNNING\n' "$PROJECT" a-waiting "$ADDRESS"
sudo -n systemctl show "$UNIT" --property=ActiveState --property=SubState \
  --property=Result --property=ExecMainStatus --property=MainPID --property=NRestarts
[ "$(incus_local config get a-waiting boot.autostart --project "$PROJECT")" = false ] || die 'Incus autostart enabled'
# A dedicated persistent slice includes all service retries, unlike per-invocation
# service counters. Incus daemon CPU/network are outside this accounting scope.
property() { sudo -n systemctl show "$SLICE" --property="$1" --value; }
# Zero bytes are evidence only after a positive control proves kernel accounting.
# curl sends loopback packets even when this low localhost port refuses connection.
control_ingress_before="$(property IPIngressBytes)"
control_egress_before="$(property IPEgressBytes)"
CONTROL_ARMED=1
timeout --foreground --kill-after=5 15 sudo -n systemd-run --quiet --wait --collect \
  --unit="$CONTROL_UNIT" --slice="$SLICE" \
  --description="$MARKER IP accounting control" --property=IPAccounting=yes \
  "$(command -v curl)" -q --noproxy '*' --max-time 2 --silent http://127.0.0.1:1/ \
  >/dev/null 2>&1 || true
control_ingress_after="$(property IPIngressBytes)"
control_egress_after="$(property IPEgressBytes)"
IP_ACCOUNTING_VERIFIED=0
if [[ "$control_ingress_before" =~ ^[0-9]+$ && "$control_ingress_after" =~ ^[0-9]+$ \
  && "$control_egress_before" =~ ^[0-9]+$ && "$control_egress_after" =~ ^[0-9]+$ ]] \
  && (( control_ingress_after > control_ingress_before || control_egress_after > control_egress_before )); then
  IP_ACCOUNTING_VERIFIED=1
  printf 'evidence: IP accounting positive control ingress_delta=%s egress_delta=%s\n' \
    "$((control_ingress_after - control_ingress_before))" "$((control_egress_after - control_egress_before))"
fi
cpu_before="$(property CPUUsageNSec)"
ingress_before="$(property IPIngressBytes)"
egress_before="$(property IPEgressBytes)"
restarts_before="$(sudo -n systemctl show "$UNIT" --property=NRestarts --value)"
sleep 65
cpu_after="$(property CPUUsageNSec)"
ingress_after="$(property IPIngressBytes)"
egress_after="$(property IPEgressBytes)"
restarts_after="$(sudo -n systemctl show "$UNIT" --property=NRestarts --value)"
state_is a-waiting STOPPED || die 'waiting yard started without its address'
state_is z-ready RUNNING || die 'independent ready yard stopped'
(( restarts_after - restarts_before >= 2 )) || die 'production retry interval did not produce two retries'
[[ "$cpu_before" =~ ^[0-9]+$ && "$cpu_after" =~ ^[0-9]+$ ]] || die 'CPU accounting is unavailable'
cpu_delta=$((cpu_after - cpu_before))
(( cpu_delta >= 0 && cpu_delta <= 2000000000 )) || die "waiting exceeded 2 CPU seconds over 65 seconds: $cpu_delta ns"
printf 'evidence: waiting=65s retries=%s cpu_ns=%s memory_bytes=%s\n' \
  "$((restarts_after - restarts_before))" "$cpu_delta" "$(property MemoryCurrent)"
if [ "$IP_ACCOUNTING_VERIFIED" = 1 ] && [[ "$ingress_before" =~ ^[0-9]+$ && "$ingress_after" =~ ^[0-9]+$ \
  && "$egress_before" =~ ^[0-9]+$ && "$egress_after" =~ ^[0-9]+$ ]]; then
  (( ingress_after == ingress_before && egress_after == egress_before )) || die 'waiting reconciler emitted network traffic'
  printf 'evidence: reconciler-slice ingress_delta=0 egress_delta=0 (Incus daemon excluded)\n'
else
  printf 'evidence: IP accounting unsupported or positive control failed; network bytes unverified (Incus daemon excluded)\n'
fi
sudo -n ip address add "$ADDRESS/32" dev "$INTERFACE"
wait_for 45 state_is a-waiting RUNNING
wait_for 15 bash -c '[ "$(sudo -n systemctl show "$1" --property=ActiveState --value)" = inactive ] && [ "$(sudo -n systemctl show "$1" --property=Result --value)" = success ]' _ "$UNIT"
printf 'ok: missing local proxy address waits cheaply, independent yard starts, address addition restores desired power\n'
