#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROJECT=subyard-p0-real-incus
MARKER=agent-e2e-p0
CONTAINER_IMAGE="${P0_REAL_INCUS_CONTAINER_IMAGE:-images:debian/13/cloud}"
VM_IMAGE="${P0_REAL_INCUS_VM_IMAGE:-images:debian/13/cloud}"
CONTAINER_CACHE_ALIAS="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-container}"
VM_CACHE_ALIAS="${P0_REAL_INCUS_VM_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-vm}"
TMP=''

die() { printf 'p0-real-incus: %s\n' "$*" >&2; exit 2; }
# Incus create/init/launch may consume YAML from stdin. The P0 lane is reached through SSH, so an
# inherited non-TTY stream can stay open forever after the operation itself succeeds.
real_incus() { timeout --foreground "${P0_REAL_INCUS_COMMAND_TIMEOUT:-900}" incus "$@" </dev/null; }
real_incus_quiet() { real_incus "$@" >/dev/null; }
project_exists() { real_incus project show "$PROJECT" >/dev/null 2>&1; }

real_incus_observe() {
  P0_REAL_INCUS_COMMAND_TIMEOUT="${P0_REAL_INCUS_OBSERVE_TIMEOUT:-30}" real_incus "$@"
}

p0_monotonic_seconds() {
  local uptime
  read -r uptime _ < /proc/uptime
  printf '%s\n' "${uptime%%.*}"
}

cleanup_real_incus() {
  local deadline="$1" now remaining
  shift
  now="$(p0_monotonic_seconds)"
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || return 124
  if [ "$remaining" -lt "${P0_REAL_INCUS_OBSERVE_TIMEOUT:-30}" ]; then
    P0_REAL_INCUS_COMMAND_TIMEOUT="$remaining" real_incus "$@"
  else
    real_incus_observe "$@"
  fi
}

cleanup_sleep() {
  local deadline="$1" requested="$2" now remaining delay
  [ "$requested" -gt 0 ] || return 0
  now="$(p0_monotonic_seconds)"
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || return 124
  delay="$requested"
  [ "$delay" -le "$remaining" ] || delay="$remaining"
  sleep "$delay"
}

active_instance_operation_ids() {
  local deadline="$1" name="$2" operations resource
  resource="/1.0/instances/$name"
  if operations="$(
    cleanup_real_incus "$deadline" operation list --project "$PROJECT" --format json
  )"; then
    :
  else
    return
  fi
  jq -r --arg resource "$resource" '
    if type != "array" or any(.[];
      type != "object" or
      (.id | type) != "string" or .id == "" or
      (.status_code | type) != "number" or .status_code < 100 or
      (.resources | type) != "object" or
      ((.resources.instances? // []) | type) != "array" or
      ((.resources.instances? // []) | any(.[]; type != "string")))
    then error("unexpected Incus operation inventory")
    else
      .[]
      | select(.status_code < 200)
      | select(any((.resources.instances? // [])[]; . == $resource))
      | .id
    end
  ' <<<"$operations"
}

delete_marked_instance() {
  local name="$1" instance_marker instance_names operation_ids project_marker now
  local delete_attempt=0
  local wait_seconds="${P0_REAL_INCUS_CLEANUP_WAIT_SECONDS:-900}"
  local poll_seconds="${P0_REAL_INCUS_CLEANUP_POLL_SECONDS:-5}"
  local delete_retry_seconds="${P0_REAL_INCUS_DELETE_RETRY_SECONDS:-2}"
  local deadline
  [[ "$wait_seconds" =~ ^[1-9][0-9]*$ ]] && [ "$wait_seconds" -le 900 ] \
    || die "invalid real-Incus cleanup wait: $wait_seconds"
  [[ "$poll_seconds" =~ ^[0-9]+$ ]] && [ "$poll_seconds" -le 30 ] \
    || die "invalid real-Incus cleanup poll interval: $poll_seconds"
  [[ "$delete_retry_seconds" =~ ^[0-9]+$ ]] && [ "$delete_retry_seconds" -le 30 ] \
    || die "invalid real-Incus delete retry interval: $delete_retry_seconds"
  now="$(p0_monotonic_seconds)"
  deadline=$((now + wait_seconds))
  if project_marker="$(
    cleanup_real_incus "$deadline" project get "$PROJECT" user.subyard.p0
  )"; then
    [ "$project_marker" = "$MARKER" ] \
      || die "refusing to clean unmarked project $PROJECT"
  else
    return
  fi
  while :; do
    now="$(p0_monotonic_seconds)"
    [ "$now" -lt "$deadline" ] \
      || die "could not settle marked instance $name within ${wait_seconds}s"
    if instance_names="$(
      cleanup_real_incus "$deadline" list "$name" --project "$PROJECT" -f csv -c n
    )"; then
      case "$instance_names" in
        '') ;;
        "$name") ;;
        *) return 2 ;;
      esac
    else
      return
    fi
    if operation_ids="$(active_instance_operation_ids "$deadline" "$name")"; then
      :
    else
      return
    fi
    if [ -n "$operation_ids" ]; then
      printf '  [warn] waiting for active Incus operation on %s: %s\n' \
        "$name" "$(tr '\n' ' ' <<<"$operation_ids")"
      cleanup_sleep "$deadline" "$poll_seconds" || return
      continue
    fi
    [ -n "$instance_names" ] || return 0
    if instance_marker="$(
      cleanup_real_incus "$deadline" config get "$name" user.subyard.p0 --project "$PROJECT"
    )"; then
      [ "$instance_marker" = "$MARKER" ] \
        || die "refusing to delete unmarked instance $name"
    else
      return
    fi
    delete_attempt=$((delete_attempt + 1))
    if cleanup_real_incus "$deadline" delete \
      "$name" --project "$PROJECT" --force >/dev/null; then
      continue
    fi
    [ "$delete_attempt" -lt 3 ] || die "could not delete marked instance $name after 3 attempts"
    # A failed Incus VM stop can leave AppArmor teardown/reload briefly in
    # flight. Keep recovery bounded and revalidate ownership before every try.
    printf '  [warn] cleanup delete of %s failed; retrying (%s/3)\n' \
      "$name" "$((delete_attempt + 1))"
    cleanup_sleep "$deadline" "$delete_retry_seconds" || return
  done
}

run_with_progress() {
  local label="$1" interval="${E2E_PROGRESS_INTERVAL:-10}" ticker rc started=$SECONDS
  shift
  printf '  [ .. ] %s\n' "$label"
  (
    while sleep "$interval"; do
      printf '  [ .. ] %s (still working, %ss elapsed)\n' "$label" "$((SECONDS - started))"
    done
  ) &
  ticker=$!
  if "$@"; then rc=0; else rc=$?; fi
  kill "$ticker" 2>/dev/null || true
  wait "$ticker" 2>/dev/null || true
  return "$rc"
}

launch_with_retry() {
  local name="$1" label="$2" attempt=1
  shift 2
  while ! run_with_progress "$label" "$@"; do
    [ "$attempt" -lt 3 ] || return 1
    printf '  [warn] %s failed; cleaning the marked instance and retrying (%s/3)\n' \
      "$label" "$((attempt + 1))"
    delete_marked_instance "$name"
    attempt=$((attempt + 1))
  done
}

cleanup() {
  local name
  if project_exists; then
    [ "$(real_incus project get "$PROJECT" user.subyard.p0 2>/dev/null)" = "$MARKER" ] \
      || die "refusing to clean unmarked project $PROJECT"
    for name in p0-container p0-vm; do delete_marked_instance "$name"; done
    [ -z "$(real_incus list --project "$PROJECT" -f csv -c n)" ] \
      || die "unexpected instance remains in $PROJECT"
    real_incus project delete "$PROJECT" >/dev/null
  fi
  if [ -n "$TMP" ] && [[ "$TMP" = /tmp/subyard-p0-incus.* ]] && [ -d "$TMP" ]; then
    find "$TMP" -depth -delete
  fi
}
trap cleanup EXIT

[ -n "${SUBYARD_E2E_VM:-}" ] || die 'run through dev/agent-e2e.sh'
for command in go incus jq sudo; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'passwordless sudo is required in a disposable test VM'
[ -S /var/lib/incus/unix.socket ] || die 'Incus socket is unavailable'
project_exists && cleanup
if cache_info="$(real_incus image info "$CONTAINER_CACHE_ALIAS" --project default 2>/dev/null)"; then
  printf '%s\n' "$cache_info" | grep -Fqx 'Type: container' \
    || die "provisioned image alias $CONTAINER_CACHE_ALIAS is not a container image"
  CONTAINER_IMAGE="$CONTAINER_CACHE_ALIAS"
  printf '  [ ok ] using provisioned real-Incus container image %s\n' "$CONTAINER_CACHE_ALIAS"
fi
if cache_info="$(real_incus image info "$VM_CACHE_ALIAS" --project default 2>/dev/null)"; then
  printf '%s\n' "$cache_info" | grep -Fqx 'Type: virtual-machine' \
    || die "provisioned image alias $VM_CACHE_ALIAS is not a VM image"
  VM_IMAGE="$VM_CACHE_ALIAS"
  printf '  [ ok ] using provisioned real-Incus VM image %s\n' "$VM_CACHE_ALIAS"
fi

real_incus project create "$PROJECT" \
  -c features.images=false -c features.profiles=false -c features.storage.volumes=false >/dev/null
real_incus project set "$PROJECT" user.subyard.p0="$MARKER"
launch_real_container() {
  real_incus_quiet launch "$CONTAINER_IMAGE" p0-container --project "$PROJECT" --storage default \
    -c user.subyard.p0="$MARKER"
}
launch_with_retry p0-container "launching real Incus container (first use may download an image)" \
  launch_real_container
if ! real_incus image info "$CONTAINER_CACHE_ALIAS" --project default >/dev/null 2>&1; then
  container_fingerprint="$(real_incus config get p0-container volatile.base_image --project "$PROJECT")"
  [[ "$container_fingerprint" =~ ^[0-9a-f]{64}$ ]] \
    || die 'real-Incus container base image fingerprint is invalid'
  real_incus image alias create "$CONTAINER_CACHE_ALIAS" "$container_fingerprint" --project default
  printf '  [ ok ] retained test-owned container image alias %s\n' "$CONTAINER_CACHE_ALIAS"
fi
launch_real_vm() {
  real_incus_quiet launch "$VM_IMAGE" p0-vm --vm --project "$PROJECT" \
    --storage default \
    -c limits.cpu=1 -c limits.memory=1GiB -c security.secureboot=false \
    -c user.subyard.p0="$MARKER" \
    -d root,size=5GiB
}
launch_with_retry p0-vm "launching real Incus VM (a clean allocation may download an image)" \
  launch_real_vm

wait_ready() {
  local name="$1" kind="$2" state='' replaced=0
  local restart_grace="${P0_REAL_INCUS_RESTART_GRACE_ATTEMPTS:-30}"
  printf '  [ .. ] waiting for %s\n' "$name"
  for _ in $(seq 1 120); do
    if real_incus exec "$name" --project "$PROJECT" -- true >/dev/null 2>&1; then
      return 0
    fi
    state="$(real_incus list "$name" --project "$PROJECT" --format csv -c s)"
    if [ "$state" = STOPPED ]; then
      printf '  [ .. ] %s is stopped; waiting for a bounded first-boot restart\n' "$name"
      for _ in $(seq 1 "$restart_grace"); do
        sleep 1
        if real_incus exec "$name" --project "$PROJECT" -- true >/dev/null 2>&1; then
          return 0
        fi
        state="$(real_incus list "$name" --project "$PROJECT" --format csv -c s)"
        [ "$state" = STOPPED ] || break
      done
      [ "$state" = STOPPED ] || continue
      if [ "$kind" = virtual-machine ] && [ "$replaced" = 0 ]; then
        printf '  [warn] %s stopped during first boot; replacing it once\n' "$name"
        delete_marked_instance "$name"
        run_with_progress "relaunching real Incus VM after first-boot stop" launch_real_vm
        replaced=1
      else
        die "$name stopped before becoming ready"
      fi
    fi
    sleep 2
  done
  die "$name did not become ready (last state: ${state:-unknown})"
}
wait_ready p0-container container
wait_ready p0-vm virtual-machine

TMP="$(mktemp -d /tmp/subyard-p0-incus.XXXXXX)"
go test -c -tags realincus -o "$TMP/incusclient-real.test" ./internal/adapters/incusclient
sudo -n env \
  SUBYARD_REAL_INCUS_SOCKET=/var/lib/incus/unix.socket \
  SUBYARD_REAL_INCUS_CONTAINER_PROJECT="$PROJECT" \
  SUBYARD_REAL_INCUS_CONTAINER_INSTANCE=p0-container \
  SUBYARD_REAL_INCUS_VM_PROJECT="$PROJECT" \
  SUBYARD_REAL_INCUS_VM_INSTANCE=p0-vm \
  SUBYARD_REAL_INCUS_TEST_BINARY="$TMP/incusclient-real.test" \
  bash "$ROOT/tests/real-host/incus-contract.sh"
cleanup
trap - EXIT
project_exists && die "$PROJECT remains after cleanup"
printf 'ok: real Incus resources passed and were removed\n'
