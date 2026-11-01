#!/usr/bin/env bash
# Current-worktree acceptance: delayed host proxy address across a real disposable-VM reboot.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# The FIFO keeps run_guest in this shell, retaining its owned worktree map.
# Receiver failure drains the stream and never acknowledges incomplete bytes.
proxy_run_guest() {
  local role="$1" rc=0 capture_rc=0 wire_rc=0 receiver wire_pid fifo decode_fifo wire
  shift
  if [ "$role" = clean ] && { [ "$PROXY_CAPTURE_ACK" != 1 ] || [ "$PROXY_CAPTURE_FAILED" != 0 ]; }; then
    printf 'E2E_PUBLICATION_CAPTURE state=refused fixture_cleanup=skipped disposable_release=controller-owned\n' >&2
    return 3
  fi
  PROXY_CAPTURE_ACK=0
  if [ "$role" != clean ]; then PROXY_WORKLOAD_EXIT=unknown; PROXY_WORKLOAD_PHASE="$role"; fi
  fifo="$LOCAL_TEMP/publication-$role.fifo"
  decode_fifo="$LOCAL_TEMP/publication-$role.decode.fifo"
  wire="$PROXY_CAPTURE_DIRECTORY/$role.capture.wire"
  mkfifo -m 0600 "$fifo" "$decode_fifo" || { PROXY_CAPTURE_FAILED=1; return 3; }
  (set -o noclobber; : > "$wire") && chmod 0600 "$wire" || { PROXY_CAPTURE_FAILED=1; return 3; }
  PROXY_CAPTURE_WIRE="$wire"
  python3 "$ROOT/dev/e2e/publication-capture.py" receive \
    "$PROXY_CAPTURE_DIRECTORY/$role.capture.tar.gz" "$PROXY_CAPTURE_ENGINE" "$PROXY_CONTROLLER_TOKEN" < "$decode_fifo" &
  receiver=$!
  PROXY_CAPTURE_RECEIVER_PID="$receiver"
  tee "$wire" < "$fifo" > "$decode_fifo" &
  wire_pid=$!
  PROXY_CAPTURE_WIRE_PID="$wire_pid"
  exec {PROXY_CAPTURE_SAVED_STDOUT}>&1
  run_guest --raw-stdout "$@" > "$fifo" || rc=$?
  exec {PROXY_CAPTURE_SAVED_STDOUT}>&-
  PROXY_CAPTURE_SAVED_STDOUT=''
  if [ "$role" != clean ]; then PROXY_WORKLOAD_EXIT="$rc"; else PROXY_FIXTURE_CLEANUP_EXIT="$rc"; fi
  wait "$wire_pid" || wire_rc=$?
  PROXY_CAPTURE_WIRE_PID=''
  wait "$receiver" || capture_rc=$?
  PROXY_CAPTURE_RECEIVER_PID=''
  [ "$wire_rc" = 0 ] || capture_rc=3
  rm -f -- "$fifo" "$decode_fifo" || capture_rc=3
  proxy_sync_wire "$wire" || capture_rc=3
  if [ "$capture_rc" = 0 ] && [ -f "$PROXY_CAPTURE_DIRECTORY/$role.capture.tar.gz" ]; then
    PROXY_CAPTURE_ACK=1
  elif [ "$role" = handoff ] || [ "$role" = resume ] || { [ "$rc" != 0 ] && [ "$role" != clean ]; }; then capture_rc=3; fi
  if [ "$capture_rc" != 0 ]; then PROXY_CAPTURE_FAILED=1; fi
  printf 'E2E_PUBLICATION_TRANSPORT phase=%s operation_exit=%s capture_exit=%s wire_exit=%s\n' "$role" "$rc" "$capture_rc" "$wire_rc" >&2
  [ "$rc" != 0 ] || [ "$capture_rc" = 0 ] || rc=3
  return "$rc"
}

# Retain partial wire bytes on interruption as well as decoder refusal.
proxy_sync_wire() {
  python3 - "$1" <<'PY_SYNC'
import os,sys
fd=os.open(sys.argv[1],os.O_RDONLY|os.O_NOFOLLOW)
os.fchmod(fd,0o600)
os.fsync(fd)
os.close(fd)
fd=os.open(os.path.dirname(sys.argv[1]),os.O_DIRECTORY|os.O_NOFOLLOW)
os.fsync(fd)
os.close(fd)
PY_SYNC
}

controller_main() {
  local slot='' before_boot='' after_boot=''
  local down=0 up=0 handoff=0
  # EXIT can run after errexit has unwound this function's local scope.
  # Keep only the trap's owned fixture state in the top-level shell.
  PROXY_CONTROLLER_BUNDLE='' PROXY_CONTROLLER_BUNDLE_HASH=''
  PROXY_CONTROLLER_TOKEN='' PROXY_CONTROLLER_PREPARED=0
  PROXY_CAPTURE_ACK=0 PROXY_CAPTURE_FAILED=0
  PROXY_CAPTURE_DIRECTORY='' PROXY_CAPTURE_ENGINE=''
  PROXY_CAPTURE_RECEIVER_PID='' PROXY_CAPTURE_WIRE_PID='' PROXY_CAPTURE_WIRE='' PROXY_CAPTURE_SAVED_STDOUT=''
  PROXY_WORKLOAD_EXIT=unknown PROXY_WORKLOAD_PHASE=none PROXY_FIXTURE_CLEANUP_EXIT=not-run
  if [ "$#" = 3 ] && [ "$3" = --publication-handoff ]; then handoff=1; set -- "$1" "$2"; fi
  [ "$#" = 2 ] && [ "$1" = --slot ] || {
    printf 'Usage: dev/e2e/proxy-address-boot.sh --slot N [--publication-handoff]\n' >&2; return 2;
  }
  slot="$2"
  # shellcheck source=dev/agent-e2e.sh
  . "$ROOT/dev/agent-e2e.sh"
  command -v python3 >/dev/null && command -v mkfifo >/dev/null && command -v tee >/dev/null || die 'publication capture requires python3, mkfifo and tee'
  set_requested_slot "$slot" --slot
  # Consumed by the sourced lease client.
  # shellcheck disable=SC2034
  VM_COUNT_REQUESTED=1 LEASE_PURPOSE=proxy-address-boot
  LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
  controller_cleanup() {
    local rc=$? failed=0
    trap - EXIT INT TERM
    set +e
    if [ -n "$PROXY_CAPTURE_SAVED_STDOUT" ]; then
      exec 1>&"$PROXY_CAPTURE_SAVED_STDOUT"
      exec {PROXY_CAPTURE_SAVED_STDOUT}>&-
      PROXY_CAPTURE_SAVED_STDOUT=''
    fi
    if [ -n "${PROXY_CAPTURE_WIRE_PID:-}" ]; then
      kill -TERM "$PROXY_CAPTURE_WIRE_PID" 2>/dev/null
      wait "$PROXY_CAPTURE_WIRE_PID" 2>/dev/null
      PROXY_CAPTURE_WIRE_PID=''
      PROXY_CAPTURE_FAILED=1
    fi
    if [ -n "$PROXY_CAPTURE_RECEIVER_PID" ]; then
      kill -TERM "$PROXY_CAPTURE_RECEIVER_PID" 2>/dev/null
      wait "$PROXY_CAPTURE_RECEIVER_PID" 2>/dev/null
      PROXY_CAPTURE_RECEIVER_PID=''
      PROXY_CAPTURE_FAILED=1
    fi
    if [ -n "${PROXY_CAPTURE_WIRE:-}" ]; then
      proxy_sync_wire "$PROXY_CAPTURE_WIRE" || PROXY_CAPTURE_FAILED=1
    fi
    if [ "$PROXY_CONTROLLER_PREPARED" = 1 ]; then
      cleanup_guest 1 quiet >/dev/null 2>&1 || failed=1
      if [ "$PROXY_CAPTURE_ACK" = 1 ] && [ "$PROXY_CAPTURE_FAILED" = 0 ]; then
        proxy_run_guest clean 1 "$PROXY_CONTROLLER_BUNDLE" "$PROXY_CONTROLLER_BUNDLE_HASH" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$PROXY_CONTROLLER_BUNDLE_HASH" \
          bash dev/e2e/proxy-address-boot.sh clean "$PROXY_CONTROLLER_TOKEN" || failed=1
      else
        printf 'E2E_PUBLICATION_CAPTURE state=refused fixture_cleanup=skipped disposable_release=controller-owned\n' >&2
        failed=1
      fi
    fi
    printf 'E2E_PUBLICATION_CONTROLLER workload_phase=%s workload_exit=%s capture_failed=%s fixture_cleanup_exit=%s\n' \
      "$PROXY_WORKLOAD_PHASE" "$PROXY_WORKLOAD_EXIT" "$PROXY_CAPTURE_FAILED" "$PROXY_FIXTURE_CLEANUP_EXIT" >&2
    [ "$failed" = 0 ] || [ "$rc" != 0 ] || rc=3
    cleanup_on_exit '' "$rc"
  }
  trap controller_cleanup EXIT
  # Signal this process group to interrupt an active foreground SSH transport.
  # A PID-only signal is handled when that foreground command returns.
  trap 'exit 130' INT
  trap 'exit 143' TERM
  PROXY_CONTROLLER_BUNDLE="$LOCAL_TEMP/worktree.tar.gz"
  build_bundle "$ROOT" "$PROXY_CONTROLLER_BUNDLE"
  PROXY_CONTROLLER_BUNDLE_HASH="$(sha256sum "$PROXY_CONTROLLER_BUNDLE" | awk '{print $1}')"
  printf 'evidence: public_bundle_sha256=%s\n' "$PROXY_CONTROLLER_BUNDLE_HASH"
  PROXY_CAPTURE_DIRECTORY="$(python3 "$ROOT/dev/e2e/publication-capture.py" directory "$ROOT" "${SUBYARD_E2E_LOCAL_EVIDENCE:-}")"
  PROXY_CAPTURE_ENGINE="$(python3 "$ROOT/dev/e2e/publication-capture.py" engine-hash "$PROXY_CONTROLLER_BUNDLE")"
  acquire_lease
  start_lease_keeper
  PROXY_CONTROLLER_TOKEN="$LEASE_GENERATION"
  [[ "$PROXY_CONTROLLER_TOKEN" =~ ^[1-9][0-9]*$ ]] || die 'invalid lease generation'
  PROXY_CONTROLLER_PREPARED=1
  proxy_run_guest prepare 1 "$PROXY_CONTROLLER_BUNDLE" "$PROXY_CONTROLLER_BUNDLE_HASH" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$PROXY_CONTROLLER_BUNDLE_HASH" \
    bash dev/e2e/proxy-address-boot.sh prepare "$PROXY_CONTROLLER_TOKEN"
  if [ "$handoff" = 1 ]; then
    proxy_run_guest handoff 1 "$PROXY_CONTROLLER_BUNDLE" "$PROXY_CONTROLLER_BUNDLE_HASH" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$PROXY_CONTROLLER_BUNDLE_HASH" \
      bash dev/e2e/proxy-address-boot.sh handoff "$PROXY_CONTROLLER_TOKEN"
    proxy_run_guest clean 1 "$PROXY_CONTROLLER_BUNDLE" "$PROXY_CONTROLLER_BUNDLE_HASH" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$PROXY_CONTROLLER_BUNDLE_HASH" \
      bash dev/e2e/proxy-address-boot.sh clean "$PROXY_CONTROLLER_TOKEN"
    cleanup_guest 1
    PROXY_CONTROLLER_PREPARED=0
    ok 'actual Go owner callback and installed passive publication handoff passed'
    exit 0
  fi
  cleanup_guest 1
  before_boot="$(ssh -F "$CLIENT_CONFIG" -T e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id)"
  timeout --foreground 20 ssh -F "$CLIENT_CONFIG" -T \
    -o ConnectTimeout=3 -o ConnectionAttempts=1 -o ServerAliveInterval=2 \
    -o ServerAliveCountMax=2 e2e-vm-1 -- sudo -n systemctl reboot \
    </dev/null >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    if ! timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=2 -o ConnectionAttempts=1 e2e-vm-1 -- true \
      </dev/null >/dev/null 2>&1; then down=1; break; fi
    sleep 1
  done
  [ "$down" = 1 ] || die 'VM did not go down for reboot'
  for _ in $(seq 1 180); do
    after_boot="$(timeout --foreground 6 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=3 -o ConnectionAttempts=1 e2e-vm-1 -- \
      cat /proc/sys/kernel/random/boot_id 2>/dev/null)" || after_boot=''
    if [ -n "$after_boot" ] && [ "$after_boot" != "$before_boot" ]; then up=1; break; fi
    sleep 1
  done
  [ "$up" = 1 ] || die 'VM did not return with a new boot ID'
  printf 'evidence: preboot=%s postboot=%s\n' "$before_boot" "$after_boot"
  proxy_run_guest resume 1 "$PROXY_CONTROLLER_BUNDLE" "$PROXY_CONTROLLER_BUNDLE_HASH" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$PROXY_CONTROLLER_BUNDLE_HASH" \
    bash dev/e2e/proxy-address-boot.sh resume "$PROXY_CONTROLLER_TOKEN"
  proxy_run_guest clean 1 "$PROXY_CONTROLLER_BUNDLE" "$PROXY_CONTROLLER_BUNDLE_HASH" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$PROXY_CONTROLLER_BUNDLE_HASH" \
    bash dev/e2e/proxy-address-boot.sh clean "$PROXY_CONTROLLER_TOKEN"
  cleanup_guest 1
  PROXY_CONTROLLER_PREPARED=0
  ok 'current-worktree runtime restored desired power automatically after delayed-address reboot'
  exit 0
}

case "${SUBYARD_E2E_VM:-}" in
  1|2) ;;
  *) controller_main "$@"; exit ;;
esac

# Reuse disposable operator, guarded runtime snapshot/restore and marker-owned project cleanup.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
if [ -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
  CANDIDATE_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
fi
SOURCE_BUNDLE_SHA256="${SUBYARD_E2E_PROXY_SOURCE_SHA256:-}"
SOURCE_BUNDLE_STATE="$STATE_ROOT/source-bundle.sha256"
RUNTIME_ARCHIVE="$RELEASE_ROOT/subyard-$CANDIDATE_VERSION-linux-amd64.tar.gz"
RUNTIME_DIGEST_STATE="$STATE_ROOT/runtime-artifact.sha256"
RUNTIME_MANIFEST="$STATE_ROOT/runtime-files.expected.sha256"
ADDRESS=192.0.2.123
INTERFACE="sypb$TOKEN"
READY=z-ready
WAITING=a-required
FOREIGN_PROJECT="sypub-foreign-$TOKEN"
FOREIGN_INSTANCE=control
PUBLIC_IMAGE=images:debian/13/cloud
OWNER_ADDRESS=100.64.200.123
OWNER_PORT=38475
PREVIEW_UNIT="subyard-e2e-preview-$TOKEN.service"
ADDRESS_UNIT="$MARKER-address.service"
ADDRESS_UNIT_PATH="/etc/systemd/system/$ADDRESS_UNIT"
ADDRESS_SCRIPT="$STATE_ROOT/deliver-address.sh"

owned_address_unit() {
  sudo -n test -f "$ADDRESS_UNIT_PATH" && sudo -n test ! -L "$ADDRESS_UNIT_PATH" \
    && [ "$(sudo -n head -n 1 "$ADDRESS_UNIT_PATH")" = "# $MARKER" ] \
    && [ "$(sudo -n stat -c %u:%g "$ADDRESS_UNIT_PATH")" = 0:0 ]
}
cleanup_preview_payload() {
  local payload
  payload="$(cat <<'CLEAN_PREVIEW'
marker="$1"; unit="$2"; root="/var/lib/$marker-preview"; path="/etc/systemd/system/$unit"
if [ -e "$root" ] || [ -L "$root" ]; then
  [ -d "$root" ] && [ ! -L "$root" ] && [ "$(stat -c %u:%g:%a "$root")" = 0:0:755 ] || exit 1
  [ -f "$root/.marker" ] && [ ! -L "$root/.marker" ] && [ "$(stat -c %u:%g:%a "$root/.marker")" = 0:0:600 ] && [ "$(cat "$root/.marker")" = "$marker" ] || exit 1
fi
if [ -e "$path" ] || [ -L "$path" ]; then
  [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -c %u:%g:%a "$path")" = 0:0:644 ] || exit 1
  [ "$(head -n 1 "$path")" = "# $marker" ] || exit 1
  systemctl disable --now "$unit"
  find "$path" -delete
  systemctl daemon-reload
fi
if [ -d "$root" ]; then find "$root" -xdev -depth -delete; fi
CLEAN_PREVIEW
  )"
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -eu -c "$payload" sh "$MARKER" "$PREVIEW_UNIT"
}
proxy_cleanup() {
  local rc=$? failed=0 project_presence_rc=0
  if [ "$rc" = 0 ] && [ "$PRESERVE_FIXTURE" = 1 ]; then cleanup; fi
  trap - EXIT INT TERM
  set +e
  if [ "$MODE" != clean ] && [ "$CLEANUP_ARMED" = 1 ]; then
    # Stream original bytes, then leave the fixture intact until the controller
    # has verified and fsynced its private artifact. Separate clean is the ack.
    local capture_rc=0
    sudo -n python3 "$ROOT/dev/e2e/publication-capture.py" collect "$ROOT" "$TOKEN" "$ADDRESS_UNIT" || capture_rc=$?
    printf 'E2E_PUBLICATION_OUTCOME workload_exit=%s capture_exit=%s fixture_cleanup=deferred\n' "$rc" "$capture_rc" >&2
    [ "$rc" != 0 ] || [ "$capture_rc" = 0 ] || rc=3
    exit "$rc"
  fi
  if [ "$rc" != 0 ]; then
    sudo -n systemctl show subyard-power-reconcile.service "$ADDRESS_UNIT" \
      -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus >&2
    sudo -n journalctl -b -u subyard-power-reconcile.service -u "$ADDRESS_UNIT" \
      --no-pager -n 40 -o cat >&2
  fi
  if [ "$CLEANUP_ARMED" = 1 ]; then
    assert_state_root
    drain_publication_worker || { printf 'proxy-address-boot: passive consumer did not drain; preserving fixture\n' >&2; exit 3; }
    project_presence || project_presence_rc=$?
    if [ "$project_presence_rc" = 0 ]; then
      capture_owned_project_images "$INSTANCE" "$READY" "$WAITING" || {
        printf 'proxy-address-boot: unbound isolated image cleanup; preserving fixture\n' >&2
        exit 3
      }
    elif [ "$project_presence_rc" != 1 ]; then
      printf 'proxy-address-boot: project absence is unknown; preserving fixture\n' >&2
      exit 3
    fi
    # A stopped or inaccessible guest is removed by the owning project cleanup.
    # Never start it for cleanup; a running guest permits exact payload removal.
    if [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s 2>/dev/null)" = RUNNING ]; then
      if [ "$(incus project get "$PROJECT" user.subyard.p0-power-systemd)" = "$MARKER" ]; then
        cleanup_preview_payload || failed=1
      else failed=1; fi
    fi
    if sudo -n test -e "$ADDRESS_UNIT_PATH" || sudo -n test -L "$ADDRESS_UNIT_PATH"; then
      if owned_address_unit; then
        sudo -n systemctl disable --now "$ADDRESS_UNIT" >/dev/null 2>&1 || failed=1
        sudo -n find "$ADDRESS_UNIT_PATH" -delete || failed=1
        sudo -n systemctl daemon-reload || failed=1
      else failed=1; fi
    fi
    if ip link show dev "$INTERFACE" >/dev/null 2>&1; then
      if [ "$(ip -j link show dev "$INTERFACE" | jq -r '.[0].ifalias')" = "$MARKER" ]; then
        sudo -n ip link delete dev "$INTERFACE" || failed=1
      else failed=1; fi
    fi
    if incus project show "$FOREIGN_PROJECT" >/dev/null 2>&1; then
      if [ "$(incus project get "$FOREIGN_PROJECT" user.subyard.p0-publication-foreign)" = "$MARKER" ]; then
        if incus info "$FOREIGN_INSTANCE" --project "$FOREIGN_PROJECT" >/dev/null 2>&1; then
          if [ "$(incus config get "$FOREIGN_INSTANCE" user.subyard.e2e --project "$FOREIGN_PROJECT")" = "$MARKER" ] \
            && [ "$(incus config get "$FOREIGN_INSTANCE" user.subyard.managed --project "$FOREIGN_PROJECT")" != true ]; then
            incus delete "$FOREIGN_INSTANCE" --project "$FOREIGN_PROJECT" --force || failed=1
          else failed=1; fi
        fi
        if [ "$(incus list --project "$FOREIGN_PROJECT" --format json | jq length)" = 0 ]; then
          incus project delete "$FOREIGN_PROJECT" || failed=1
        else failed=1; fi
      else failed=1; fi
    fi
    for cleanup_instance in "$READY" "$WAITING"; do
    if incus info "$cleanup_instance" --project "$PROJECT" >/dev/null 2>&1; then
      if [ "$(incus config get "$cleanup_instance" user.subyard.e2e --project "$PROJECT")" = "$MARKER" ]; then
        sudo -n systemctl stop subyard-power-reconcile.service || failed=1
        incus delete "$cleanup_instance" --project "$PROJECT" --force || failed=1
      else failed=1; fi
    fi
    done
  fi
  [ "$failed" = 0 ] || rc=3
  (exit "$rc")
  cleanup
}
trap proxy_cleanup EXIT

ensure_platform() {
  local platform_root="$HOME/.cache/subyard-e2e-platform" marker temporary
  marker="$platform_root/.subyard-e2e-platform-marker"
  if [ -e "$marker" ] || [ -L "$marker" ]; then
    [ -f "$marker" ] && [ ! -L "$marker" ] \
      && [ "$(cat "$marker")" = subyard-e2e-platform-v1 ] || die 'unsafe platform marker'
  fi
  if ! incus info >/dev/null 2>&1 \
    || ! incus storage show default --project default >/dev/null 2>&1 \
    || ! incus network show incusbr0 --project default >/dev/null 2>&1; then
    (
      bootstrap="$(mktemp -d /tmp/subyard-proxy-boot-platform.XXXXXXXX)"
      trap 'sudo -n find "$bootstrap" -depth -delete' EXIT
      # shellcheck source=tests/helpers/test-context.sh
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$bootstrap/context"
      export SUBYARD_USER
      SUBYARD_USER="$(id -un)"
      export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
      export SUBYARD_CONFIG_HOME="$bootstrap/config" SUBYARD_HOME="$platform_root"
      export STORAGE_PATH="$platform_root/incus/incus/storage"
      export HOST_BASE="$bootstrap/host-data" RESTRICTED_DISK_PATHS="$bootstrap/host-data"
      set -a
      # shellcheck source=config/host.env
      . "$ROOT/config/host.env"
      set +a
      timeout --foreground --kill-after=10 300 bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
    )
  fi
  timeout --foreground 180 sudo -n incus admin waitready >/dev/null
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  if [ ! -e "$marker" ]; then
    install -d -m 0711 "$platform_root"
    temporary="$(mktemp "$platform_root/.platform-marker.XXXXXX")"
    printf '%s\n' subyard-e2e-platform-v1 > "$temporary"
    chmod 0600 "$temporary"
    mv -f -- "$temporary" "$marker"
  fi
}

assert_guards() {
  # Use the installed candidate's read-only guard implementation.
  operator_env env SUBYARD_SUDO_PREAUTHORIZED=1 bash -c '
    set -euo pipefail
    . "$1/scripts/lib-power.sh"
    power_nm_prepare_reader
    power_host_safe incusbr0 || { printf "%s\n" "$POWER_ERROR" >&2; exit 1; }
  ' _ "$OPERATOR_HOME/.subyard/runtime/current"
  printf 'evidence: candidate NetworkManager and default-route guards passed\n'
}
assert_source() {
  [[ "$SOURCE_BUNDLE_SHA256" =~ ^[0-9a-f]{64}$ ]] || die 'source bundle checksum is required'
  [ "$(sha256sum "$ROOT/../worktree.tar.gz" | awk '{print $1}')" = "$SOURCE_BUNDLE_SHA256" ] \
    || die 'transported worktree does not match the controller source bundle'
}
assert_release() {
  local target snapshot digest
  assert_source
  [ "$(read_fixture_value "$SOURCE_BUNDLE_STATE")" = "$SOURCE_BUNDLE_SHA256" ] \
    || die 'source bundle changed across reboot'
  digest="$(read_fixture_value "$RUNTIME_DIGEST_STATE")"
  [ "$(sha256sum "$RUNTIME_ARCHIVE" | awk '{print $1}')" = "$digest" ] \
    || die 'candidate artifact changed across reboot'
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'unexpected installed candidate'
  target="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  [ "$target" = "releases/$CANDIDATE_VERSION-${digest:0:12}" ] \
    && [ "$target" = "$(read_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE")" ] \
    || die 'installed runtime differs from the transported candidate artifact'
  operator_env cmp "$RUNTIME_MANIFEST" "$OPERATOR_HOME/.subyard/runtime/$target/runtime-files.sha256" \
    || die 'installed runtime manifest differs from the candidate artifact'
  operator_env bash -c 'cd "$1" && sha256sum -c runtime-files.sha256 >/dev/null' _ \
    "$OPERATOR_HOME/.subyard/runtime/$target" || die 'installed runtime checksum mismatch'
  operator_env cmp "$OPERATOR_HOME/.subyard/runtime/$target/bin/yard-engine" "$RECONCILER" \
    || die 'boot runtime differs from the candidate engine'
  for path in "$RECONCILER" "$UNIT" "$PUBLICATION_UNIT"; do
    sudo -n test -f "$path" && sudo -n test ! -L "$path" \
      && [ "$(sudo -n stat -c %u:%g "$path")" = 0:0 ] || die 'boot runtime is not root-owned'
  done
  [ "$(sudo -n stat -c %a "$RECONCILER")" = 755 ] \
    && [ "$(sudo -n stat -c %a "$UNIT")" = 644 ] \
    && [ "$(sudo -n stat -c %a "$PUBLICATION_UNIT")" = 644 ] || die 'unexpected boot runtime permissions'
  assert_unit_matches "$OPERATOR_HOME/.subyard/runtime/$target/config/systemd/subyard-power-reconcile.service.in" loaded candidate
  snapshot="$(sudo -n systemctl show subyard-power-reconcile.service -p FragmentPath -p DropInPaths)"
  grep -Fxq "FragmentPath=$UNIT" <<<"$snapshot" \
    && grep -Fxq 'DropInPaths=' <<<"$snapshot" || die 'unexpected boot unit override'
  for name in "$INSTANCE" "$READY" "$WAITING"; do
    [ "$(incus config get "$name" boot.autostart --project "$PROJECT")" = false ] \
      && [ "$(incus config get "$name" user.subyard.desired_power --project "$PROJECT")" = running ] \
      || die 'desired power or Incus autostart changed'
  done
  printf 'evidence: source_bundle_sha256=%s candidate_version=%s runtime_artifact_sha256=%s boot_runtime_sha256=%s root_owned=yes persistent_unit=yes\n' \
    "$SOURCE_BUNDLE_SHA256" "$CANDIDATE_VERSION" "$digest" \
    "$(sudo -n sha256sum "$RECONCILER" | awk '{print $1}')"
  assert_guards
}

pin_original_publication_instance() {
  local uuid path="$STATE_ROOT/publication-physical.uuid"
  uuid="$(incus query "/1.0/instances/$INSTANCE?project=$PROJECT" | jq -er --arg name "$INSTANCE" '
    select(.name == $name and .type == "container" and .status == "Running"
      and .config["user.subyard.managed"] == "true"
      and .config["user.subyard.initialized"] == "true")
    | .config["volatile.uuid"] | select(type == "string" and
      test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))')" \
    || die 'original publication instance identity is unavailable'
  if [ "${1:-}" = capture ] && [ ! -e "$path" ] && [ ! -L "$path" ]; then
    write_fixture_value "$path" "$uuid"
  fi
  [ "$(read_fixture_value "$path")" = "$uuid" ] \
    || die 'original publication instance identity changed'
}

optional_route() {
  local host="$1" runtime_root
  runtime_root="$OPERATOR_HOME/.subyard/runtime/$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  operator_env bash -eu -c '
    . "$1/scripts/lib/preview-proxy.sh"
    YARD_INSTANCE_NAME="$2" INCUS_PROJECT="$3" WEB_PREVIEW_HOST_PORT="$4" YARD_KIND=container
    PROJ=(--project "$INCUS_PROJECT")
    subyard_preview_route "$5"
  ' _ "$runtime_root" "$INSTANCE" "$PROJECT" "$OWNER_PORT" "$host"
}

optional_reconcile() {
  local rc=0 contracts
  contracts="$(sudo -n sed -n 's/^Environment=SUBYARD_PUBLICATION_CONTRACTS=//p' "$UNIT")"
  sudo -n env SUBYARD_PUBLICATION_CONTRACTS="$contracts" "$RECONCILER" _power-reconcile || rc=$?
  [ "$rc" = 0 ] || [ "$rc" = 75 ] || die 'optional reconcile failed'
}

wait_local_preview() {
  local deadline=$((SECONDS+60))
  until incus exec "$INSTANCE" --project "$PROJECT" -- curl -q --noproxy '*' --max-time 3 --fail --silent http://127.0.0.1:8765/ | grep -Fx "$MARKER" >/dev/null; do
    if (( SECONDS >= deadline )); then
      incus exec "$INSTANCE" --project "$PROJECT" -- curl -q --noproxy '*' --max-time 3 --silent \
        --output /dev/null --write-out 'evidence: local_preview_http_status=%{http_code}\n' http://127.0.0.1:8765/ >&2 || true
      incus exec "$INSTANCE" --project "$PROJECT" -- systemctl show "$PREVIEW_UNIT" \
        -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus -p MainPID >&2 || true
      die 'local preview did not become reachable'
    fi
    sleep 1
  done
}

assert_optional_restored() {
  local boot_file="$1" pid_file="$2" deadline=$((SECONDS+60)) snapshot route_ready=0 http_ready=0
  while :; do
    snapshot="$(incus query "/1.0/instances/$INSTANCE?project=$PROJECT")" || {
      printf 'evidence: owner_route_query=failed\n' >&2
      die 'owner route was broadened or ownership did not converge'
    }
    if jq -e --arg host "$OWNER_ADDRESS" --arg port "$OWNER_PORT" '
    .status == "Running" and .config["user.subyard.desired_power"] == "running"
    and .config["user.subyard.deferred_publication.subyard-preview"] == null
    and .devices["subyard-preview"] == {type:"proxy",bind:"host",listen:("tcp:"+$host+":"+$port),connect:"tcp:127.0.0.1:8765"}
    and .config["user.subyard.preview_proxy"] == ("v1:"+$host+":"+$port)
    ' <<<"$snapshot" >/dev/null 2>&1; then
      route_ready=1
      if curl -q --noproxy '*' --connect-timeout 2 --max-time 3 --fail --silent "http://$OWNER_ADDRESS:$OWNER_PORT/" | grep -Fx "$MARKER" >/dev/null; then
        http_ready=1; break
      fi
    else
      route_ready=0
    fi
    (( SECONDS < deadline )) || break
    sleep 1
  done
  if [ "$route_ready" != 1 ]; then
    jq -c --arg host "$OWNER_ADDRESS" --arg port "$OWNER_PORT" '
      if type != "object" or (.config | type) != "object" or (.devices | type) != "object"
         or (.expanded_devices != null and (.expanded_devices | type) != "object") then
        {owner_route_snapshot:"invalid-shape"}
      else
        {type:"proxy",bind:"host",listen:("tcp:"+$host+":"+$port),connect:"tcp:127.0.0.1:8765"} as $expected
        | .config["user.subyard.deferred_publication.subyard-preview"] as $deferred
        | {owner_route_snapshot:"parsed", running:(.status == "Running"),
           desired_running:(.config["user.subyard.desired_power"] == "running"),
           deferred_absent:($deferred == null),
           deferred_class:(if $deferred == null then "absent" elif ($deferred | type) != "string" then "invalid-type" elif $deferred == "" then "empty" else "present" end),
           local_device_exact:(.devices["subyard-preview"] == $expected),
           effective_device_exact:((.expanded_devices // .devices)["subyard-preview"] == $expected),
           ownership_exact:(.config["user.subyard.preview_proxy"] == ("v1:"+$host+":"+$port))}
      end
    ' <<<"$snapshot" >&2 2>/dev/null || printf 'evidence: owner_route_snapshot=parse-failed\n' >&2
    die 'owner route was broadened or ownership did not converge'
  fi
  [ "$http_ready" = 1 ] || die 'exact owner endpoint never became reachable'
  incus exec "$INSTANCE" --project "$PROJECT" -- cat /proc/sys/kernel/random/boot_id > "$STATE_ROOT/current-optional-boot-id"
  incus exec "$INSTANCE" --project "$PROJECT" -- systemctl show "$PREVIEW_UNIT" -p MainPID --value > "$STATE_ROOT/current-optional-preview-pid"
  cmp -s "$boot_file" "$STATE_ROOT/current-optional-boot-id" || die 'publication restore restarted the yard'
  cmp -s "$pid_file" "$STATE_ROOT/current-optional-preview-pid" || die 'publication restore restarted the local preview workload'
}

prepare_optional_publication() {
  local payload
  ! ip -j address show | jq -e --arg address "$OWNER_ADDRESS" 'any(.[].addr_info[]; .local == $address)' >/dev/null || die 'synthetic owner address already exists'
  sudo -n ip address add "$OWNER_ADDRESS/32" dev "$INTERFACE"
  payload="$(cat <<'GUEST'
marker="$1"; unit="$2"
command -v python3 >/dev/null
command -v curl >/dev/null
[ ! -e "/etc/systemd/system/$unit" ] && [ ! -L "/etc/systemd/system/$unit" ] || exit 1
root="/var/lib/$marker-preview"
if [ -e "$root" ] || [ -L "$root" ]; then
  [ -d "$root" ] && [ ! -L "$root" ] && [ "$(stat -c %u:%g:%a "$root")" = 0:0:755 ] || exit 1
  [ -f "$root/.marker" ] && [ ! -L "$root/.marker" ] && [ "$(stat -c %u:%g:%a "$root/.marker")" = 0:0:600 ] && [ "$(cat "$root/.marker")" = "$marker" ] || exit 1
fi
[ ! -L "$root/index.html" ] || exit 1
install -d -m 0755 "$root"
printf '%s\n' "$marker" > "$root/.marker"
chmod 0600 "$root/.marker"
printf '%s\n' "$marker" > "$root/index.html"
chmod 0644 "$root/index.html"
{
 printf '# %s\n[Unit]\nDescription=%s preview workload\nAfter=network.target\n' "$marker" "$marker"
 printf '[Service]\nExecStart=/usr/bin/python3 -m http.server 8765 --bind 127.0.0.1 --directory %s\n' "$root"
 printf '[Install]\nWantedBy=multi-user.target\n'
} | install -o root -g root -m 0644 /dev/stdin "/etc/systemd/system/$unit"
systemctl daemon-reload
systemctl enable --now "$unit"
GUEST
  )"
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -eu -c "$payload" sh "$MARKER" "$PREVIEW_UNIT"
  wait_local_preview
  # Stop before introducing the synthetic route: it intentionally differs
  # from configured preview readiness. Start observes the stopped guest normally.
  operator_yard stop --force --yes
  optional_route "$OWNER_ADDRESS"
  sudo -n ip address del "$OWNER_ADDRESS/32" dev "$INTERFACE"
  operator_yard start --yes
  incus query "/1.0/instances/$INSTANCE?project=$PROJECT" | jq -e '
    .status == "Running" and .config["user.subyard.desired_power"] == "running"
    and .devices["subyard-preview"] == null
    and .config["user.subyard.deferred_publication.subyard-preview"] != null' >/dev/null || die 'explicit start did not defer only the optional listener'
  wait_local_preview
  incus exec "$INSTANCE" --project "$PROJECT" -- cat /proc/sys/kernel/random/boot_id > "$STATE_ROOT/explicit-optional-boot-id"
  incus exec "$INSTANCE" --project "$PROJECT" -- systemctl show "$PREVIEW_UNIT" -p MainPID --value > "$STATE_ROOT/explicit-optional-preview-pid"
  optional_reconcile
  optional_reconcile
  # Contradictory saved ownership fails without stopping the local workload.
  sudo -n systemctl stop subyard-power-reconcile.service
  incus config set "$INSTANCE" user.subyard.preview_proxy "v1:$OWNER_ADDRESS:38476" --project "$PROJECT"
  local conflict_rc=0 contracts
  contracts="$(sudo -n sed -n 's/^Environment=SUBYARD_PUBLICATION_CONTRACTS=//p' "$UNIT")"
  # The fixture operator owns STATE_ROOT; only the reconciler process needs root.
  # shellcheck disable=SC2024
  sudo -n env SUBYARD_PUBLICATION_CONTRACTS="$contracts" "$RECONCILER" _power-reconcile > "$STATE_ROOT/publication-conflict.log" 2>&1 || conflict_rc=$?
  [ "$conflict_rc" = 1 ] || die 'saved ownership drift was accepted'
  [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = RUNNING ] || die 'ownership conflict stopped the local yard'
  incus exec "$INSTANCE" --project "$PROJECT" -- curl -q --noproxy '*' --fail --silent http://127.0.0.1:8765/ | grep -Fx "$MARKER" >/dev/null
  incus config set "$INSTANCE" user.subyard.preview_proxy "v1:$OWNER_ADDRESS:$OWNER_PORT" --project "$PROJECT"
  sudo -n systemctl reset-failed subyard-power-reconcile.service
  sudo -n systemctl start --no-block subyard-power-reconcile.service
  optional_reconcile
  local ssh_endpoint ssh_port
  ssh_endpoint="$(incus config device get "$INSTANCE" ssh listen --project "$PROJECT")"
  [[ "$ssh_endpoint" =~ ^tcp:127\.0\.0\.1:([0-9]+)$ ]] || die 'local SSH endpoint changed'
  ssh_port="${BASH_REMATCH[1]}"
  timeout --foreground 5 bash -c 'exec 3<>"/dev/tcp/127.0.0.1/$1"; read -r banner <&3; [[ "$banner" = SSH-2.0-* ]]' _ "$ssh_port" || die 'local SSH listener was unavailable'
  operator_yard status | grep -F 'publication waiting for address:' >/dev/null || die 'status omitted pending publication'
  sudo -n ip address add "$OWNER_ADDRESS/32" dev "$INTERFACE"
  assert_optional_restored "$STATE_ROOT/explicit-optional-boot-id" "$STATE_ROOT/explicit-optional-preview-pid"
  optional_reconcile
  # A second disappearance and cancellation must never resurrect the saved route.
  sudo -n ip address del "$OWNER_ADDRESS/32" dev "$INTERFACE"
  optional_reconcile
  optional_route 127.0.0.1
  sudo -n ip address add "$OWNER_ADDRESS/32" dev "$INTERFACE"
  optional_reconcile
  incus query "/1.0/instances/$INSTANCE?project=$PROJECT" | jq -e '
    .status == "Running" and .devices["subyard-preview"] == null
    and .config["user.subyard.preview_proxy"] == null
    and .config["user.subyard.deferred_publication.subyard-preview"] == null' >/dev/null || die 'cancelled route was resurrected'
  optional_route "$OWNER_ADDRESS"
  printf 'evidence: explicit_start=RUNNING missing_owner_ip=deferred local_preview=reachable exact_restore=reachable local_ssh=reachable guest_identity=unchanged cancellation=preserved ownership_drift=fail-closed\n'
}

prepare_proxy() {
  [[ "$TOKEN" =~ ^[0-9]{1,10}$ ]] || die 'token is too long for dummy interface'
  assert_source
  ensure_platform
  incus list --all-projects --format json | jq -e \
    'all(.[]; (.expanded_config["user.subyard.managed"] // .config["user.subyard.managed"] // "false") != "true")' \
    >/dev/null || die 'unexpected managed yards on disposable host'
  local project_presence_rc=0
  project_presence || project_presence_rc=$?
  [ "$project_presence_rc" = 1 ] || die 'fresh fixture project is present or its absence is unknown'
  ! ip link show dev "$INTERFACE" >/dev/null 2>&1 || die 'dummy interface already exists'
  ! ip -j address show | jq -e --arg address "$ADDRESS" \
    'any(.[].addr_info[]; .local == $address)' >/dev/null || die 'synthetic address already exists'
  sudo -n test ! -e "$ADDRESS_UNIT_PATH" && sudo -n test ! -L "$ADDRESS_UNIT_PATH" \
    || die 'address unit already exists'
  prepare_fixture
  write_fixture_value "$SOURCE_BUNDLE_STATE" "$SOURCE_BUNDLE_SHA256"
  p0_capacity_reset_build_cache
  if [ -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
    # shellcheck source=tests/helpers/release-candidate.sh
    . "$ROOT/tests/helpers/release-candidate.sh"
    release_candidate_prepare "$ROOT" >/dev/null
    install -d -m 0755 "$RELEASE_ROOT"
    cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
  else
    info "packaging transported current worktree as $CANDIDATE_VERSION"
    "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" \
      --version "$CANDIDATE_VERSION" >/dev/null
  fi
  chmod -R a+rX "$RELEASE_ROOT"
  write_fixture_value "$RUNTIME_DIGEST_STATE" "$(sha256sum "$RUNTIME_ARCHIVE" | awk '{print $1}')"
  tar -xOzf "$RUNTIME_ARCHIVE" ./runtime-files.sha256 > "$RUNTIME_MANIFEST"
  chmod 0644 "$RUNTIME_MANIFEST"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes
  write_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE" \
    "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
  operator_yard config --yes set YARD_IMAGE "$PUBLIC_IMAGE" --scope yard --local
  operator_yard config --yes set WEB_PREVIEW_HOST_PORT "$OWNER_PORT" --scope yard --local
  operator_yard config --yes set YARD_IMAGE_FALLBACK "$PUBLIC_IMAGE" --scope yard --local
  operator_yard init --yes
  [ "$(incus project get "$PROJECT" user.subyard.installation)" = 1 ] \
    && [ "$(incus project get "$PROJECT" user.subyard.operator_uid)" = "$(operator_uid)" ] \
    && [ "$(incus project get "$PROJECT" user.subyard.data_home)" = "$OPERATOR_HOME/.subyard" ] \
    && [ "$(incus project get "$PROJECT" user.subyard.config_home)" = "$OPERATOR_HOME/.config/subyard" ] \
    || die 'ordinary init did not establish exact product project ownership'
  incus project set "$PROJECT" user.subyard.p0-power-systemd="$MARKER"
  operator_yard start --yes
  pin_original_publication_instance capture
  sudo -n ip link add "$INTERFACE" type dummy
  sudo -n ip link set dev "$INTERFACE" alias "$MARKER"
  sudo -n ip link set dev "$INTERFACE" up
  sudo -n ip address add "$ADDRESS/32" dev "$INTERFACE"
  incus init "$PUBLIC_IMAGE" "$WAITING" --project "$PROJECT" --storage default \
    -c user.subyard.e2e="$MARKER" -c boot.autostart=false \
    -c user.subyard.managed=true -c user.subyard.initialized=true \
    -c user.subyard.desired_power=running -c user.subyard.name="$WAITING" \
    -c user.subyard.bridge=incusbr0 >/dev/null
  incus config device add "$WAITING" delayed-address proxy --project "$PROJECT" \
    listen="tcp:$ADDRESS:38473" connect=tcp:127.0.0.1:22 bind=host nat=false >/dev/null
  incus start "$WAITING" --project "$PROJECT"
  incus init "$PUBLIC_IMAGE" "$READY" --project "$PROJECT" --storage default \
    -c user.subyard.e2e="$MARKER" -c boot.autostart=false \
    -c user.subyard.managed=true -c user.subyard.initialized=true \
    -c user.subyard.desired_power=running -c user.subyard.name="$READY" \
    -c user.subyard.bridge=incusbr0 >/dev/null
  incus config device add "$READY" address-test proxy --project "$PROJECT" \
    listen=tcp:127.0.0.1:38474 connect=tcp:127.0.0.1:22 bind=host nat=false >/dev/null
  incus start "$READY" --project "$PROJECT"
  prepare_optional_publication
  assert_release
  record_reboot_baseline
  {
    printf '# %s\n' "$MARKER"
    printf 'set -euo pipefail\n'
    for name in MARKER STATE_ROOT INTERFACE ADDRESS OWNER_ADDRESS OWNER_PORT PREVIEW_UNIT PROJECT INSTANCE READY WAITING BOOT_ID_STATE; do
      printf '%s=%q\n' "$name" "${!name}"
    done
    cat <<'DELIVER'
[ "$(cat "$STATE_ROOT/.marker")" = "$MARKER" ]
[ "$(cat /proc/sys/kernel/random/boot_id)" != "$(cat "$BOOT_ID_STATE")" ]
! ip link show dev "$INTERFACE" >/dev/null 2>&1
! ip -j address show | jq -e --arg address "$ADDRESS" 'any(.[].addr_info[]; .local == $address)' >/dev/null
ip link add "$INTERFACE" type dummy
ip link set dev "$INTERFACE" alias "$MARKER"
ip link set dev "$INTERFACE" up
for _ in $(seq 1 150); do
  snapshot="$(systemctl show subyard-power-reconcile.service -p SubState -p Result -p ExecMainStatus -p MainPID)"
  if journalctl -b -u subyard-power-reconcile.service --no-pager -o cat \
      | grep -F "subyard-power: waiting-for-address $PROJECT/$WAITING: $ADDRESS" >/dev/null \
    && grep -Fxq 'SubState=auto-restart' <<<"$snapshot" \
    && grep -Fxq 'Result=success' <<<"$snapshot" \
    && grep -Fxq 'ExecMainStatus=75' <<<"$snapshot" \
    && grep -Fxq 'MainPID=0' <<<"$snapshot" \
    && [ "$(incus list "$WAITING" --project "$PROJECT" --format csv -c s)" = STOPPED ] \
    && [ "$(incus list "$READY" --project "$PROJECT" --format csv -c s)" = RUNNING ] \
    && [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = RUNNING ] \
    && incus exec "$INSTANCE" --project "$PROJECT" -- curl -q --noproxy '*' --max-time 3 --fail --silent http://127.0.0.1:8765/ | grep -Fx "$MARKER" >/dev/null; then
    printf '%s\n' "$snapshot" \
      | install -o root -g root -m 0644 /dev/stdin "$STATE_ROOT/waiting.properties"
    incus exec "$INSTANCE" --project "$PROJECT" -- curl -q --noproxy '*' --fail --silent http://127.0.0.1:8765/ | grep -Fx "$MARKER" >/dev/null
    incus query "/1.0/instances/$INSTANCE?project=$PROJECT" | jq -e '.status == "Running" and .config["user.subyard.desired_power"] == "running" and .devices["subyard-preview"] == null and .config["user.subyard.deferred_publication.subyard-preview"] != null' >/dev/null
    incus exec "$INSTANCE" --project "$PROJECT" -- cat /proc/sys/kernel/random/boot_id > "$STATE_ROOT/optional-boot-id"
    incus exec "$INSTANCE" --project "$PROJECT" -- systemctl show "$PREVIEW_UNIT" -p MainPID --value > "$STATE_ROOT/optional-preview-pid"
    printf '%s\n' "waiting-for-address $PROJECT/$WAITING: $ADDRESS optional-running=$PROJECT/$INSTANCE local-preview=reachable independent-ready=RUNNING" \
      | install -o root -g root -m 0644 /dev/stdin "$STATE_ROOT/waiting.proof"
    sleep 35
    [ "$(incus list "$WAITING" --project "$PROJECT" --format csv -c s)" = STOPPED ]
    [ "$(incus list "$READY" --project "$PROJECT" --format csv -c s)" = RUNNING ]
    [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = RUNNING ]
    ip address add "$OWNER_ADDRESS/32" dev "$INTERFACE"
    ip address add "$ADDRESS/32" dev "$INTERFACE"
    printf '%s\n' "$MARKER" \
      | install -o root -g root -m 0644 /dev/stdin "$STATE_ROOT/address-delivered"
    exit 0
  fi
  sleep 1
done
printf 'delayed-address boot never reached expected waiting state\n' >&2
exit 1
DELIVER
  } | sudo -n install -o root -g root -m 0700 /dev/stdin "$ADDRESS_SCRIPT"
  {
    printf '# %s\n' "$MARKER"
    printf '[Unit]\nDescription=Marker-owned delayed proxy address\nAfter=incus.service incus.socket\nWants=incus.service incus.socket\n'
    printf '[Service]\nType=oneshot\nExecStart=/bin/bash %s\nTimeoutStartSec=300\nRemainAfterExit=yes\n' "$ADDRESS_SCRIPT"
    printf '[Install]\nWantedBy=multi-user.target\n'
  } | sudo -n install -o root -g root -m 0644 /dev/stdin "$ADDRESS_UNIT_PATH"
  sudo -n systemctl daemon-reload
  sudo -n systemctl enable "$ADDRESS_UNIT"
  write_fixture_value "$PHASE_STATE" proxy-ready
  PRESERVE_FIXTURE=1
  ok 'current-worktree runtime and marker-owned delayed address are ready for reboot'
}

resume_proxy() {
  local snapshot='' finished=0 route="$STATE_ROOT/route-after"
  assert_state_root
  CLEANUP_ARMED=1
  assert_fixture_phase proxy-ready
  [ "$(cat /proc/sys/kernel/random/boot_id)" != "$(read_fixture_value "$BOOT_ID_STATE")" ] \
    || die 'fixture did not cross a VM reboot'
  for _ in $(seq 1 240); do
    snapshot="$(sudo -n systemctl show subyard-power-reconcile.service \
      -p ActiveState -p SubState -p Result -p ExecMainStatus -p ExecMainStartTimestampMonotonic)"
    if [ "$(cat "$STATE_ROOT/address-delivered" 2>/dev/null)" = "$MARKER" ] \
      && grep -Fxq 'ActiveState=inactive' <<<"$snapshot" \
      && grep -Fxq 'SubState=dead' <<<"$snapshot" \
      && grep -Fxq 'Result=success' <<<"$snapshot" \
      && grep -Fxq 'ExecMainStatus=0' <<<"$snapshot" \
      && grep -Eq '^ExecMainStartTimestampMonotonic=[1-9][0-9]*$' <<<"$snapshot" \
      && [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = RUNNING ]; then
      finished=1; break
    fi
    [ "$(sudo -n systemctl show "$ADDRESS_UNIT" -p ActiveState --value)" != failed ] \
      || die 'delayed address service failed'
    sleep 1
  done
  [ "$finished" = 1 ] || die 'installed boot reconciler never restored RUNNING'
  cat "$STATE_ROOT/waiting.proof" "$STATE_ROOT/waiting.properties"
  printf '%s\n' "$snapshot"
  sudo -n journalctl -b -u subyard-power-reconcile.service --no-pager -o cat
  [ "$(incus list "$READY" --project "$PROJECT" --format csv -c s)" = RUNNING ] \
    || die 'independent ready yard stopped'
  ip -4 route show default > "$route"
  cmp -s "$DEFAULT_ROUTE_STATE" "$route" || die 'host default route changed across reboot'
  assert_release
  assert_optional_restored "$STATE_ROOT/optional-boot-id" "$STATE_ROOT/optional-preview-pid"
  printf 'ok: owned optional publication preserved RUNNING/local preview across delayed-address reboot; required foreign wait and independent yard preserved\n'
}

run_publication_handoff() {
  local installed binary="$STATE_ROOT/publication-physical.test" log="$STATE_ROOT/publication-physical.jsonl"
  assert_state_root
  CLEANUP_ARMED=1
  PRESERVE_FIXTURE=0
  assert_fixture_phase proxy-ready
  assert_release
  # A separate unregistered project is outside ordinary teardown inventory.
  # Its exact marker permits cleanup; it carries no managed yard registration.
  ! incus project show "$FOREIGN_PROJECT" >/dev/null 2>&1 || die 'foreign-control project already exists'
  incus project create "$FOREIGN_PROJECT" -c features.images=false -c features.profiles=false \
    -c user.subyard.p0-publication-foreign="$MARKER" >/dev/null
  ! incus info "$FOREIGN_INSTANCE" --project "$FOREIGN_PROJECT" >/dev/null 2>&1 || die 'foreign-control instance already exists'
  incus init "$PUBLIC_IMAGE" "$FOREIGN_INSTANCE" --project "$FOREIGN_PROJECT" --storage default \
    -c user.subyard.e2e="$MARKER" -c boot.autostart=false >/dev/null
  installed="$OPERATOR_HOME/.subyard/runtime/$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  pin_original_publication_instance
  write_fixture_value "$STATE_ROOT/publication-physical.foreign.uuid" "$(incus config get "$FOREIGN_INSTANCE" volatile.uuid --project "$FOREIGN_PROJECT")"
  # Compile the real CLI package; execution uses its production client/socket.
  # No Rust source selector or test-only product build tags enter this lane.
  env -u SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT go test -c -o "$binary" ./internal/cli
  chmod 0755 "$binary"
  # The unprivileged VM operator owns STATE_ROOT; only the test process needs sudo.
  # shellcheck disable=SC2024
  (
    cd "$ROOT/internal/cli"
    sudo -n env -u GOTMPDIR -u SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT TMPDIR=/tmp \
      SUBYARD_ORIGINAL_CALLBACK_REQUIRED=1 SUBYARD_HOST_INSTALL_RESULT_SOURCE="$installed" \
      "$binary" -test.v -test.count=1 -test.timeout=3m \
      -test.run='^TestOwnerOriginalCallbackContinuesAfterInstallationImport$' \
      > "$STATE_ROOT/callback-continuation.jsonl" 2>&1
  ) || { cat "$STATE_ROOT/callback-continuation.jsonl"; return 1; }
  cat "$STATE_ROOT/callback-continuation.jsonl"
  [ "$(grep -c '^--- PASS: TestOwnerOriginalCallbackContinuesAfterInstallationImport (' "$STATE_ROOT/callback-continuation.jsonl")" = 1 ] \
    && ! grep -q -- '--- SKIP:' "$STATE_ROOT/callback-continuation.jsonl" \
    || die 'required original callback continuation case did not execute'
  sudo -n systemctl stop subyard-power-reconcile.service
  operator_env env -u GOTMPDIR -u SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT TMPDIR=/tmp \
    SUBYARD_E2E_VM="$SUBYARD_E2E_VM" SUBYARD_REPOSITORY_ROOT="$installed" SUBYARD_CONFIG_DIR="$installed/config" \
    SUBYARD_OPERATOR_HOME="$OPERATOR_HOME" SUBYARD_HOME="$OPERATOR_HOME/.subyard" SUBYARD_CONFIG_HOME="$OPERATOR_HOME/.config/subyard" \
    SUBYARD_PUBLICATION_PHYSICAL_REQUIRED=1 SUBYARD_PUBLICATION_PHYSICAL_ROOT="$installed" \
    SUBYARD_PUBLICATION_PHYSICAL_MARKER="$MARKER" SUBYARD_PUBLICATION_PHYSICAL_PROJECT="$PROJECT" \
    SUBYARD_PUBLICATION_PHYSICAL_INSTANCE="$INSTANCE" SUBYARD_PUBLICATION_PHYSICAL_FOREIGN_PROJECT="$FOREIGN_PROJECT" \
    SUBYARD_PUBLICATION_PHYSICAL_FOREIGN="$FOREIGN_INSTANCE" \
    SUBYARD_PUBLICATION_PHYSICAL_INTERFACE="$INTERFACE" SUBYARD_PUBLICATION_PHYSICAL_ADDRESS="$OWNER_ADDRESS" \
    SUBYARD_PUBLICATION_PHYSICAL_PORT="$OWNER_PORT" SUBYARD_PUBLICATION_PHYSICAL_PREVIEW="$PREVIEW_UNIT" \
    SUBYARD_SUDO_PREAUTHORIZED=1 "$binary" -test.v -test.count=1 -test.timeout=8m \
      -test.run='^TestOwnerPublicationHandoffPhysical$' > "$log" 2>&1 || { cat "$log"; return 1; }
  cat "$log"
  [ "$(grep -c '^--- PASS: TestOwnerPublicationHandoffPhysical (' "$log")" = 1 ] \
    && ! grep -q -- '--- SKIP:' "$log" || die 'required physical publication case did not execute'
  for phase in ack-tail coalesced-retry exact-restore temporary-stop serialized-stop-retirement teardown-waiting; do
    grep -Fq -- "--- PASS: TestOwnerPublicationHandoffPhysical/$phase " "$log" \
      || die "required publication phase missing: $phase"
  done
}

for command in sudo incus ip jq systemctl timeout go tar curl python3; do
  command -v "$command" >/dev/null || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
case "$MODE" in
  prepare) prepare_proxy ;;
  resume) resume_proxy ;;
  handoff) run_publication_handoff ;;
  clean)
    if [ -e "$STATE_ROOT" ]; then assert_state_root; CLEANUP_ARMED=1; fi
    ;;
  *) die 'expected prepare, resume, handoff or clean' ;;
esac
