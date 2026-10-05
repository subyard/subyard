#!/usr/bin/env bash
# Current-worktree acceptance: delayed host proxy address across a real disposable-VM reboot.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

controller_main() {
  local slot='' bundle='' bundle_hash='' token='' prepared=0 before_boot='' after_boot=''
  local down=0 up=0 rc=0 failed=0
  [ "$#" = 2 ] && [ "$1" = --slot ] || {
    printf 'Usage: dev/e2e/proxy-address-boot.sh --slot N\n' >&2; return 2;
  }
  slot="$2"
  # shellcheck source=dev/agent-e2e.sh
  . "$ROOT/dev/agent-e2e.sh"
  set_requested_slot "$slot" --slot
  # Consumed by the sourced lease client.
  # shellcheck disable=SC2034
  VM_COUNT_REQUESTED=1 LEASE_PURPOSE=proxy-address-boot
  LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
  controller_cleanup() {
    rc=$?
    trap - EXIT INT TERM
    set +e
    if [ "$prepared" = 1 ]; then
      cleanup_guest 1 quiet >/dev/null 2>&1 || failed=1
      run_guest 1 "$bundle" "$bundle_hash" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$bundle_hash" \
        bash dev/e2e/proxy-address-boot.sh clean "$token" \
        || failed=1
    fi
    [ "$failed" = 0 ] || rc=3
    (exit "$rc")
    cleanup_on_exit
  }
  trap controller_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  bundle="$LOCAL_TEMP/worktree.tar.gz"
  build_bundle "$ROOT" "$bundle"
  bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
  printf 'evidence: public_bundle_sha256=%s\n' "$bundle_hash"
  acquire_lease
  start_lease_keeper
  token="$LEASE_GENERATION"
  [[ "$token" =~ ^[1-9][0-9]*$ ]] || die 'invalid lease generation'
  prepared=1
  run_guest 1 "$bundle" "$bundle_hash" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$bundle_hash" \
    bash dev/e2e/proxy-address-boot.sh prepare "$token"
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
  run_guest 1 "$bundle" "$bundle_hash" env SUBYARD_E2E_PROXY_SOURCE_SHA256="$bundle_hash" \
    bash dev/e2e/proxy-address-boot.sh resume "$token"
  cleanup_guest 1
  prepared=0
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
ADDRESS_UNIT="$MARKER-address.service"
ADDRESS_UNIT_PATH="/etc/systemd/system/$ADDRESS_UNIT"
ADDRESS_SCRIPT="$STATE_ROOT/deliver-address.sh"

owned_address_unit() {
  sudo -n test -f "$ADDRESS_UNIT_PATH" && sudo -n test ! -L "$ADDRESS_UNIT_PATH" \
    && [ "$(sudo -n head -n 1 "$ADDRESS_UNIT_PATH")" = "# $MARKER" ] \
    && [ "$(sudo -n stat -c %u:%g "$ADDRESS_UNIT_PATH")" = 0:0 ]
}
proxy_cleanup() {
  local rc=$? failed=0
  if [ "$rc" = 0 ] && [ "$PRESERVE_FIXTURE" = 1 ]; then cleanup; fi
  trap - EXIT INT TERM
  set +e
  if [ "$rc" != 0 ]; then
    sudo -n systemctl show subyard-power-reconcile.service "$ADDRESS_UNIT" \
      -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus >&2
    sudo -n journalctl -b -u subyard-power-reconcile.service -u "$ADDRESS_UNIT" \
      --no-pager -n 40 -o cat >&2
  fi
  if [ "$CLEANUP_ARMED" = 1 ]; then
    assert_state_root
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
    if incus info "$READY" --project "$PROJECT" >/dev/null 2>&1; then
      if [ "$(incus config get "$READY" user.subyard.e2e --project "$PROJECT")" = "$MARKER" ]; then
        sudo -n systemctl stop subyard-power-reconcile.service || failed=1
        incus delete "$READY" --project "$PROJECT" --force || failed=1
      else failed=1; fi
    fi
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
  for path in "$RECONCILER" "$UNIT"; do
    sudo -n test -f "$path" && sudo -n test ! -L "$path" \
      && [ "$(sudo -n stat -c %u:%g "$path")" = 0:0 ] || die 'boot runtime is not root-owned'
  done
  [ "$(sudo -n stat -c %a "$RECONCILER")" = 755 ] \
    && [ "$(sudo -n stat -c %a "$UNIT")" = 644 ] || die 'unexpected boot runtime permissions'
  operator_env cat "$OPERATOR_HOME/.subyard/runtime/$target/config/systemd/subyard-power-reconcile.service.in" \
    > "$STATE_ROOT/candidate.service.in"
  assert_unit_matches "$STATE_ROOT/candidate.service.in" loaded 5
  snapshot="$(sudo -n systemctl show subyard-power-reconcile.service -p FragmentPath -p DropInPaths)"
  grep -Fxq "FragmentPath=$UNIT" <<<"$snapshot" \
    && grep -Fxq 'DropInPaths=' <<<"$snapshot" || die 'unexpected boot unit override'
  for name in "$INSTANCE" "$READY"; do
    [ "$(incus config get "$name" boot.autostart --project "$PROJECT")" = false ] \
      && [ "$(incus config get "$name" user.subyard.desired_power --project "$PROJECT")" = running ] \
      || die 'desired power or Incus autostart changed'
  done
  printf 'evidence: source_bundle_sha256=%s candidate_version=%s runtime_artifact_sha256=%s boot_runtime_sha256=%s root_owned=yes persistent_unit=yes\n' \
    "$SOURCE_BUNDLE_SHA256" "$CANDIDATE_VERSION" "$digest" \
    "$(sudo -n sha256sum "$RECONCILER" | awk '{print $1}')"
  assert_guards
}

prepare_proxy() {
  [[ "$TOKEN" =~ ^[0-9]{1,10}$ ]] || die 'token is too long for dummy interface'
  assert_source
  ensure_platform
  incus list --all-projects --format json | jq -e \
    'all(.[]; (.expanded_config["user.subyard.managed"] // .config["user.subyard.managed"] // "false") != "true")' \
    >/dev/null || die 'unexpected managed yards on disposable host'
  if ! incus image info subyard-e2e-debian-13-cloud-container --project default >/dev/null 2>&1; then
    timeout --foreground --kill-after=10 300 sudo -n incus image copy images:debian/13/cloud local: \
      --alias subyard-e2e-debian-13-cloud-container --project default >/dev/null
  fi
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
  incus project create "$PROJECT" -c features.images=false \
    -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
  operator_yard init --yes
  operator_yard start --yes
  sudo -n ip link add "$INTERFACE" type dummy
  sudo -n ip link set dev "$INTERFACE" alias "$MARKER"
  sudo -n ip link set dev "$INTERFACE" up
  sudo -n ip address add "$ADDRESS/32" dev "$INTERFACE"
  incus config device add "$INSTANCE" delayed-address proxy --project "$PROJECT" \
    listen="tcp:$ADDRESS:38473" connect=tcp:127.0.0.1:22 bind=host nat=false >/dev/null
  incus init subyard-e2e-debian-13-cloud-container "$READY" --project "$PROJECT" --storage default \
    -c user.subyard.e2e="$MARKER" -c boot.autostart=false \
    -c user.subyard.managed=true -c user.subyard.initialized=true \
    -c user.subyard.desired_power=running -c user.subyard.name="$READY" \
    -c user.subyard.bridge=incusbr0 >/dev/null
  incus config device add "$READY" address-test proxy --project "$PROJECT" \
    listen=tcp:127.0.0.1:38474 connect=tcp:127.0.0.1:22 bind=host nat=false >/dev/null
  incus start "$READY" --project "$PROJECT"
  assert_release
  record_reboot_baseline
  {
    printf '# %s\n' "$MARKER"
    printf 'set -euo pipefail\n'
    for name in MARKER STATE_ROOT INTERFACE ADDRESS PROJECT INSTANCE READY BOOT_ID_STATE; do
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
      | grep -F "subyard-power: waiting-for-address $PROJECT/$INSTANCE: $ADDRESS" >/dev/null \
    && grep -Fxq 'SubState=auto-restart' <<<"$snapshot" \
    && grep -Fxq 'Result=success' <<<"$snapshot" \
    && grep -Fxq 'ExecMainStatus=75' <<<"$snapshot" \
    && grep -Fxq 'MainPID=0' <<<"$snapshot" \
    && [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = STOPPED ] \
    && [ "$(incus list "$READY" --project "$PROJECT" --format csv -c s)" = RUNNING ]; then
    printf '%s\n' "$snapshot" \
      | install -o root -g root -m 0644 /dev/stdin "$STATE_ROOT/waiting.properties"
    printf '%s\n' "waiting-for-address $PROJECT/$INSTANCE: $ADDRESS independent-ready=RUNNING" \
      | install -o root -g root -m 0644 /dev/stdin "$STATE_ROOT/waiting.proof"
    sleep 35
    [ "$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)" = STOPPED ]
    [ "$(incus list "$READY" --project "$PROJECT" --format csv -c s)" = RUNNING ]
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
  printf 'ok: explicit boot waiting, independent RUNNING, delayed address and automatic RUNNING passed\n'
}

for command in sudo incus ip jq systemctl timeout go tar; do
  command -v "$command" >/dev/null || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
case "$MODE" in
  prepare) prepare_proxy ;;
  resume) resume_proxy ;;
  clean)
    if [ -e "$STATE_ROOT" ]; then assert_state_root; CLEANUP_ARMED=1; fi
    ;;
  *) die 'expected prepare, resume or clean' ;;
esac
