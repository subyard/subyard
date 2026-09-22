#!/usr/bin/env bash
# Real-host acceptance for opt-in isolation and explicit links between local yards.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

controller_main() {
  slot=''
  target_vm=1
  target_alias=''
  vm_set=0
  bundle=''
  bundle_hash=''
  token=''
  before_boot=''
  after_boot=''
  down=0
  up=0
  system_state=''
  power_snapshot=''
  power_started=''
  system_rc=0
  ready=0
  power_ready=0
  prepared=0
  cleanup_failed=0
  rc=0
  report_power_failure() {
    printf 'agent-e2e: boot power reconciler status:\n' >&2
    sed -n -E '/^(LoadState|ActiveState|SubState|Result|ExecMainStatus|ExecMainStartTimestampMonotonic)=/p' \
      <<<"$power_snapshot" >&2
    printf 'agent-e2e: boot power reconciler journal (current boot, last 40 lines):\n' >&2
    timeout --foreground 15 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=3 -o ConnectionAttempts=1 "$target_alias" -- \
      sudo -n journalctl -b -u subyard-power-reconcile.service \
        --no-pager -n 40 -o short-monotonic >&2 || true
  }
  usage() {
    printf 'Usage: dev/e2e/yard-network-policy.sh --slot N [--vm 1|2]\n' >&2
    return 2
  }
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --slot)
        [ "$#" -ge 2 ] && [ -z "$slot" ] || { usage; return 2; }
        slot="$2"
        shift 2
        ;;
      --vm)
        [ "$#" -ge 2 ] && [ "$vm_set" = 0 ] || { usage; return 2; }
        target_vm="$2"
        vm_set=1
        shift 2
        ;;
      *) usage; return 2 ;;
    esac
  done
  [ -n "$slot" ] && { [ "$target_vm" = 1 ] || [ "$target_vm" = 2 ]; } \
    || { usage; return 2; }
  target_alias="e2e-vm-$target_vm"
  # shellcheck source=dev/agent-e2e.sh
  . "$ROOT/dev/agent-e2e.sh"
  set_requested_slot "$slot" --slot
  # Consumed dynamically by the sourced lease client.
  # shellcheck disable=SC2034
  LEASE_PURPOSE='yard-network-policy'
  LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"

  controller_cleanup() {
    existing_directory=''
    clean_succeeded=0
    old_directory_cleaned=0
    rc=$?
    trap - EXIT INT TERM
    set +e
    if [ "$prepared" = 1 ] && [ -n "${bundle:-}" ] && [ -n "${bundle_hash:-}" ]; then
      existing_directory="${GUEST_DIRS[$target_vm]:-}"
      if [ -n "$existing_directory" ]; then
        case "$existing_directory" in
          /tmp/subyard-worktree.*)
            if guest "$target_vm" test -f \
              "$existing_directory/src/dev/e2e/yard-network-policy.sh" \
              >/dev/null 2>&1 \
              && guest "$target_vm" /usr/sbin/runuser -u dev -- \
                env HOME=/home/dev USER=dev LOGNAME=dev \
                SUBYARD_E2E_VM="$target_vm" SUBYARD_E2E_NETWORK_TOKEN="$token" \
                bash "$existing_directory/src/dev/e2e/yard-network-policy.sh" clean \
                >/dev/null 2>&1; then
              clean_succeeded=1
            fi
            ;;
          *) cleanup_failed=1 ;;
        esac
        if cleanup_guest "$target_vm" >/dev/null 2>&1; then
          old_directory_cleaned=1
        else
          cleanup_failed=1
        fi
      fi
      if [ "$clean_succeeded" != 1 ] \
        && { [ -z "$existing_directory" ] || [ "$old_directory_cleaned" = 1 ]; }; then
        run_guest "$target_vm" "$bundle" "$bundle_hash" env \
          SUBYARD_E2E_NETWORK_TOKEN="$token" \
          bash dev/e2e/yard-network-policy.sh clean >/dev/null 2>&1 \
          && clean_succeeded=1 \
          || cleanup_failed=1
        cleanup_guest "$target_vm" >/dev/null 2>&1 || cleanup_failed=1
      fi
      [ "$clean_succeeded" = 1 ] || cleanup_failed=1
    fi
    if [ -n "${LEASE_KEEPER_PID:-}" ]; then
      kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      LEASE_KEEPER_PID=''
    fi
    for cleanup_vm in "${!GUEST_DIRS[@]}"; do
      cleanup_guest "$cleanup_vm" >/dev/null 2>&1 || cleanup_failed=1
    done
    release_lease >/dev/null || cleanup_failed=1
    if [ -n "${LOCAL_TEMP:-}" ]; then
      case "$LOCAL_TEMP" in
        /tmp/subyard-agent-e2e.*|"${TMPDIR:-/tmp}"/subyard-agent-e2e.*)
          find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_failed=1
          ;;
      esac
    fi
    [ "$cleanup_failed" = 0 ] || rc=3
    exit "$rc"
  }
  trap controller_cleanup EXIT INT TERM

  acquire_lease
  start_lease_keeper
  bundle="$LOCAL_TEMP/worktree.tar.gz"
  info 'packing current public worktree'
  build_bundle "$ROOT" "$bundle"
  bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
  ok "worktree bundle ready (sha256=$bundle_hash)"
  run_guest "$target_vm" "$bundle" "$bundle_hash" env \
    bash dev/e2e/yard-network-policy.sh recover
  cleanup_guest "$target_vm"
  token="f${LEASE_GENERATION}"
  [[ "$token" =~ ^[a-f0-9]+$ ]] || die 'lease token is invalid'
  run_guest "$target_vm" "$bundle" "$bundle_hash" env \
    SUBYARD_E2E_NETWORK_TOKEN="$token" \
    bash dev/e2e/yard-network-policy.sh clean
  cleanup_guest "$target_vm"
  prepared=1
  run_guest "$target_vm" "$bundle" "$bundle_hash" env \
    SUBYARD_E2E_NETWORK_TOKEN="$token" \
    bash dev/e2e/yard-network-policy.sh prepare
  cleanup_guest "$target_vm"

  before_boot="$(ssh -F "$CLIENT_CONFIG" -T "$target_alias" -- \
    cat /proc/sys/kernel/random/boot_id)" \
    || die "cannot read VM$target_vm boot ID before reboot"
  set +e
  timeout --foreground 20 ssh -F "$CLIENT_CONFIG" -T \
    -o ConnectTimeout=3 -o ConnectionAttempts=1 \
    -o ServerAliveInterval=2 -o ServerAliveCountMax=2 \
    "$target_alias" -- sudo -n systemctl reboot </dev/null >/dev/null 2>&1
  set -e
  for _ in $(seq 1 60); do
    if ! timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=2 -o ConnectionAttempts=1 "$target_alias" -- true \
      </dev/null >/dev/null 2>&1; then
      down=1
      break
    fi
    sleep 1
  done
  [ "$down" = 1 ] || die "VM$target_vm did not go down for reboot"
  for _ in $(seq 1 180); do
    after_boot="$(timeout --foreground 6 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=3 -o ConnectionAttempts=1 "$target_alias" -- \
      cat /proc/sys/kernel/random/boot_id 2>/dev/null)" || after_boot=''
    if [ -n "$after_boot" ] && [ "$after_boot" != "$before_boot" ]; then
      up=1
      break
    fi
    sleep 1
  done
  [ "$up" = 1 ] || die "VM$target_vm did not return with a new boot ID"
  for _ in $(seq 1 180); do
    system_rc=0
    system_state="$(timeout --foreground 8 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=3 -o ConnectionAttempts=1 "$target_alias" -- \
      systemctl is-system-running 2>/dev/null)" || system_rc=$?
    case "$system_state:$system_rc" in
      running:0|degraded:1) ready=1; break ;;
      initializing:1|starting:1|*:255|*:124) ;;
      *) die "VM$target_vm returned in unexpected system state: ${system_state:-unknown}" ;;
    esac
    sleep 1
  done
  [ "$ready" = 1 ] || die "VM$target_vm did not reach a stable system state after reboot"
  for _ in $(seq 1 150); do
    power_snapshot="$(timeout --foreground 8 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=3 -o ConnectionAttempts=1 "$target_alias" -- \
      systemctl show subyard-power-reconcile.service \
      -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus \
      -p ExecMainStartTimestampMonotonic 2>/dev/null)" || power_snapshot=''
    power_started="$(sed -n 's/^ExecMainStartTimestampMonotonic=//p' \
      <<<"$power_snapshot")"
    if [[ "$power_started" =~ ^[1-9][0-9]*$ ]] \
      && grep -Fxq 'ActiveState=inactive' <<<"$power_snapshot" \
      && grep -Fxq 'SubState=dead' <<<"$power_snapshot" \
      && grep -Fxq 'Result=success' <<<"$power_snapshot" \
      && grep -Fxq 'ExecMainStatus=0' <<<"$power_snapshot"; then
      power_ready=1
      break
    fi
    if grep -Fxq 'ActiveState=failed' <<<"$power_snapshot"; then
      report_power_failure
      die "VM$target_vm boot power reconciler reached a terminal failure"
    fi
    grep -Eq '^LoadState=(not-found|error|bad-setting|masked)$' <<<"$power_snapshot" \
      && die "VM$target_vm boot power reconciler is unavailable"
    sleep 1
  done
  [ "$power_ready" = 1 ] \
    || die "VM$target_vm boot power reconciler did not complete after reboot"

  run_guest "$target_vm" "$bundle" "$bundle_hash" env \
    SUBYARD_E2E_NETWORK_TOKEN="$token" \
    SUBYARD_E2E_NETWORK_PREBOOT_ID="$before_boot" \
    bash dev/e2e/yard-network-policy.sh resume
  cleanup_guest "$target_vm"
  prepared=0
  ok "network isolation survived a broker-controlled VM$target_vm reboot"
  exit 0
}

case "${SUBYARD_E2E_VM:-}" in
  1|2) ;;
  *) controller_main "$@"; exit ;;
esac

STATE=''
MARKER=''
ESTABLISHED_PID=''
YARDS=()
MODE="${1:-}"
PRESERVE_SUCCESS=0
WORK_COMPLETE=0
YARD_BIN=''
baseline_bindings=''

die() { printf 'yard-network-policy-e2e: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

[ "$MODE" = prepare ] || [ "$MODE" = resume ] || [ "$MODE" = clean ] \
  || [ "$MODE" = recover ] \
  || die 'guest mode must be prepare, resume, clean, or recover'
for command in curl go incus jq sha256sum sg ss sudo timeout; do
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

yard() { "$YARD_BIN" -Y "$1" "${@:2}"; }
network() { yard "${YARDS[0]}" network "$@"; }
project() { printf 'subyard-%s\n' "$1"; }
instance() { printf 'yard-%s\n' "$1"; }
guest() {
  local name="$1"
  shift
  incus exec "$(instance "$name")" --project "$(project "$name")" -- "$@"
}

wait_incus_ready() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    timeout --foreground 180 sudo -n /usr/bin/incus admin waitready >/dev/null
  else
    timeout --foreground 180 /usr/bin/incus admin waitready >/dev/null
  fi
}

ensure_incus_platform() {
  local platform_root="$HOME/.cache/subyard-e2e-platform" marker temporary
  marker="$platform_root/.subyard-e2e-platform-marker"
  if ! command -v incus >/dev/null 2>&1 \
    || ! wait_incus_ready \
    || ! incus info >/dev/null 2>&1 \
    || ! incus storage show default --project default >/dev/null 2>&1 \
    || ! incus network show incusbr0 --project default >/dev/null 2>&1; then
    info 'reconciling the real Incus platform with the product installer'
    (
      # shellcheck source=tests/helpers/test-context.sh
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$STATE/platform-bootstrap"
      export SUBYARD_USER
      SUBYARD_USER="$(id -un)"
      export SUBYARD_OPERATOR_HOME="$HOME"
      export SUBYARD_CONFIG_DIR="$ROOT/config"
      export SUBYARD_CONFIG_HOME="$STATE/platform-bootstrap-config"
      export SUBYARD_HOME="$platform_root"
      export STORAGE_PATH="$platform_root/incus/incus/storage"
      export HOST_BASE="$STATE/platform-host-data"
      export RESTRICTED_DISK_PATHS="$HOST_BASE"
      set -a
      # shellcheck source=config/host.env
      . "$ROOT/config/host.env"
      set +a
      bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
    )
  fi
  wait_incus_ready || die 'Incus did not become ready after platform reconciliation'
  incus info >/dev/null 2>&1 || die 'Incus owner API is unavailable'
  incus storage show default --project default >/dev/null 2>&1 \
    || die 'Incus default storage pool is unavailable'
  incus network show incusbr0 --project default >/dev/null 2>&1 \
    || die 'Incus managed bridge is unavailable'
  if [ -e "$marker" ]; then
    [ "$(<"$marker")" = subyard-e2e-platform-v1 ] \
      || die "unexpected shared E2E platform marker: $marker"
  else
    install -d -m 0711 "$platform_root"
    temporary="$(mktemp "$platform_root/.platform-marker.XXXXXX")"
    printf '%s\n' subyard-e2e-platform-v1 > "$temporary"
    chmod 0600 "$temporary"
    mv -f -- "$temporary" "$marker"
  fi
}

validate_fixture_state() {
  local name config_path config_dir entry expected=0
  [ -d "$STATE" ] && [ ! -L "$STATE" ] \
    && [ "$(stat -c '%u:%a' "$STATE")" = "$(id -u):700" ] \
    || return 1
  [ -f "$STATE/.marker" ] && [ ! -L "$STATE/.marker" ] \
    && [ "$(stat -c '%u:%a' "$STATE/.marker")" = "$(id -u):600" ] \
    && [ "$(<"$STATE/.marker")" = "$MARKER" ] \
    || return 1
  [ ! -e "$STATE/.armed" ] \
    || { [ -f "$STATE/.armed" ] && [ ! -L "$STATE/.armed" ] \
      && [ "$(stat -c '%u:%a' "$STATE/.armed")" = "$(id -u):600" ] \
      && [ "$(<"$STATE/.armed")" = "$MARKER" ]; } \
    || return 1
  [ ! -e "$STATE/bindings.json" ] \
    || { [ -f "$STATE/bindings.json" ] && [ ! -L "$STATE/bindings.json" ] \
      && [ "$(stat -c '%u:%a' "$STATE/bindings.json")" = "$(id -u):600" ] \
      && jq -e '
        type == "array" and length > 0 and
        all(.[]; type == "string" and test("^[a-z0-9][a-z0-9_-]*$")) and
        . == (sort | unique)
      ' "$STATE/bindings.json" >/dev/null; } \
    || return 1
  [ ! -e "$STATE/config/yards" ] \
    || { [ -d "$STATE/config/yards" ] && [ ! -L "$STATE/config/yards" ]; } \
    || return 1
  if [ -d "$STATE/config/yards" ]; then
    while IFS= read -r entry; do
      expected=0
      for name in "${YARDS[@]}"; do
        [ "$entry" = "$name" ] && expected=1
      done
      [ "$expected" = 1 ] || return 1
    done < <(find "$STATE/config/yards" -mindepth 1 -maxdepth 1 -printf '%f\n')
  fi
  for name in "${YARDS[@]}"; do
    config_dir="$STATE/config/yards/$name"
    config_path="$STATE/config/yards/$name/config.env"
    [ ! -e "$config_path" ] && continue
    [ -d "$config_dir" ] && [ ! -L "$config_dir" ] \
      && [ "$(stat -c '%u:%a' "$config_dir")" = "$(id -u):700" ] \
      || return 1
    [ -f "$config_path" ] && [ ! -L "$config_path" ] \
      && [ "$(stat -c '%u:%a' "$config_path")" = "$(id -u):600" ] \
      && grep -Fqx "# $MARKER" "$config_path" \
      && ! grep -Eq '^(YARD_NAME|INCUS_PROJECT|YARD_INSTANCE_NAME|INCUS_BRIDGE)=' \
        "$config_path" \
      || return 1
  done
}

host_policy_empty() {
  local policy
  policy="$(incus project get default user.subyard.network_policy 2>/dev/null)" || return 1
  [ -z "$policy" ] || jq -e '
    .isolation == false and .appliedIsolation == false and
    ((.links // []) | length == 0) and ((.bindings // []) | length == 0) and
    ((.pendingStart // []) | length == 0) and ((.removing // []) | length == 0)
  ' <<<"$policy" >/dev/null
}

cleanup() {
  local rc=$? name project_name instance_name project_marker instance_marker
  local inventory mutation_armed=0 has_instance=0 cleanup_failed=0
  local project_marker_ok=0 instance_marker_ok=0 config_owned=0
  trap - EXIT INT TERM
  if [ "$rc" = 0 ] && [ "$PRESERVE_SUCCESS" = 1 ] && [ "$WORK_COMPLETE" = 1 ]; then
    exit 0
  fi
  set +e
  if [ -n "$ESTABLISHED_PID" ] && kill -0 "$ESTABLISHED_PID" 2>/dev/null; then
    kill "$ESTABLISHED_PID" 2>/dev/null
    wait "$ESTABLISHED_PID" 2>/dev/null
  fi
  if [ -f "$STATE/.armed" ] && [ "$(<"$STATE/.armed")" = "$MARKER" ]; then
    mutation_armed=1
  fi
  if ! validate_fixture_state; then
    printf 'yard-network-policy-e2e: refusing invalid persistent fixture state\n' >&2
    cleanup_failed=1
    mutation_armed=0
  fi
  if [ "$mutation_armed" = 1 ] && [ "${#YARDS[@]}" -gt 0 ] \
    && [ -x "$YARD_BIN" ]; then
    network isolation off --yes >/dev/null 2>&1 || cleanup_failed=1
    network reconcile --yes >/dev/null 2>&1 || cleanup_failed=1
  fi
  for name in "${YARDS[@]}"; do
    [ "$mutation_armed" = 1 ] || break
    project_name="$(project "$name")"
    instance_name="$(instance "$name")"
    if incus project show "$project_name" >/dev/null 2>&1; then
      project_marker=''
      project_marker_ok=0
      if project_marker="$(incus project get "$project_name" user.subyard.e2e 2>/dev/null)"; then
        project_marker_ok=1
      fi
      inventory="$(incus list --project "$project_name" --format json 2>/dev/null)" \
        || inventory=''
      if [ "$project_marker_ok" != 1 ] || [ -z "$inventory" ] \
        || ! jq -e --arg expected "$instance_name" \
          'all(.[]; .name == $expected) and (length <= 1)' \
          <<<"$inventory" >/dev/null; then
        printf 'yard-network-policy-e2e: refusing project with foreign instances %s\n' \
          "$project_name" >&2
        cleanup_failed=1
        continue
      fi
      instance_marker=''
      instance_marker_ok=1
      has_instance=0
      if jq -e 'length == 1' <<<"$inventory" >/dev/null; then
        has_instance=1
        if ! instance_marker="$(incus config get "$instance_name" user.subyard.e2e \
          --project "$project_name" 2>/dev/null)"; then
          instance_marker_ok=0
        fi
      fi
      config_owned=0
      if [ -f "$STATE/config/yards/$name/config.env" ] \
        && grep -Fqx "# $MARKER" "$STATE/config/yards/$name/config.env"; then
        config_owned=1
      fi
      if [ "$instance_marker_ok" = 1 ] \
        && [ "$project_marker" = "$MARKER" ] \
        && { [ "$has_instance" = 0 ] \
          || [ "$instance_marker" = "$MARKER" ]; }; then
        yard "$name" teardown --yes >/dev/null 2>&1 || cleanup_failed=1
      elif [ "$instance_marker_ok" = 1 ] && [ "$config_owned" = 1 ] \
        && { [ -z "$project_marker" ] || [ "$project_marker" = "$MARKER" ]; } \
        && { [ "$has_instance" = 0 ] || [ -z "$instance_marker" ] \
          || [ "$instance_marker" = "$MARKER" ]; }; then
        # A failed init can create the unique project before the test adds its second marker.
        yard "$name" teardown --yes >/dev/null 2>&1 || cleanup_failed=1
      else
        printf 'yard-network-policy-e2e: refusing ambiguous project %s\n' \
          "$project_name" >&2
        cleanup_failed=1
      fi
    fi
    if incus project show "$project_name" >/dev/null 2>&1; then
      printf 'yard-network-policy-e2e: marked teardown left project %s\n' \
        "$project_name" >&2
      cleanup_failed=1
    fi
  done
  if [ "$mutation_armed" = 0 ]; then
    for name in "${YARDS[@]}"; do
      if incus project show "$(project "$name")" >/dev/null 2>&1; then
        printf 'yard-network-policy-e2e: unarmed fixture name already exists: %s\n' \
          "$(project "$name")" >&2
        cleanup_failed=1
      fi
    done
  fi
  host_policy_empty || cleanup_failed=1
  incus network acl list --project default --format json 2>/dev/null | jq -e '
    [.[] | select(.config["user.subyard.managed"] == "true")] | length == 0
  ' >/dev/null 2>&1 || cleanup_failed=1
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-yard-network-policy.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = "$MARKER" ]; then
    if [ "$cleanup_failed" = 0 ]; then
      sudo -n find "$STATE" -depth -delete || cleanup_failed=1
    fi
  fi
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}

if [ "$MODE" = recover ]; then
  recovery_failed=0
  shopt -s nullglob
  for recovery_state in /var/tmp/subyard-yard-network-policy.*; do
    recovery_token="${recovery_state##*.}"
    if [[ ! "$recovery_token" =~ ^[a-z0-9]+$ ]]; then
      printf 'yard-network-policy-e2e: refusing invalid recovery path %s\n' \
        "$recovery_state" >&2
      recovery_failed=1
      continue
    fi
    env SUBYARD_E2E_VM="$SUBYARD_E2E_VM" \
      SUBYARD_E2E_NETWORK_TOKEN="$recovery_token" \
      bash "$ROOT/dev/e2e/yard-network-policy.sh" clean \
      || recovery_failed=1
  done
  [ "$recovery_failed" = 0 ] \
    || die 'one or more prior network fixtures require operator inspection'
  exit 0
fi

trap cleanup EXIT INT TERM

token="${SUBYARD_E2E_NETWORK_TOKEN:-}"
[[ "$token" =~ ^[a-z0-9]+$ ]] || die 'guest network token is invalid'
STATE="/var/tmp/subyard-yard-network-policy.$token"
MARKER="subyard-yard-network-policy-e2e-v1-$token"
YARDS=("net-a-$token" "net-b-$token" "net-c-$token")
YARD_BIN="$STATE/yard"
case "$MODE" in
  prepare)
    [ ! -e "$STATE" ] || die "marker-owned state already exists: $STATE"
    install -d -m 0700 "$STATE"
    printf '%s\n' "$MARKER" > "$STATE/.marker"
    chmod 0600 "$STATE/.marker"
    PRESERVE_SUCCESS=1
    ;;
  resume)
    validate_fixture_state \
      || die 'persistent network fixture state is absent or invalid'
    ;;
  clean)
    if [ ! -e "$STATE" ]; then
      trap - EXIT INT TERM
      exit 0
    fi
    validate_fixture_state \
      || die 'persistent network fixture state is absent or invalid'
    ;;
esac

ensure_incus_platform
if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
  && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
  command=''
  if [ "$MODE" = prepare ]; then
    sudo -n find "$STATE" -depth -delete
  fi
  trap - EXIT INT TERM
  printf -v command 'exec env SUBYARD_E2E_VM=%q SUBYARD_E2E_NETWORK_TOKEN=%q bash %q %q' \
    "$SUBYARD_E2E_VM" "$token" "$ROOT/dev/e2e/yard-network-policy.sh" "$MODE"
  exec sg incus-admin -c "$command"
fi

export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_REPOSITORY_ROOT="$ROOT"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
export HOST_CLAUDE_MD=
export HOST_CODEX_AGENTS_MD=
export HOST_OPENCODE_AGENTS_MD=

if [ "$MODE" = clean ]; then
  exit 0
fi

wait_guest() {
  local name="$1"
  for _ in $(seq 1 120); do
    if guest "$name" true >/dev/null 2>&1 \
      && guest "$name" ip -4 address show dev eth0 scope global \
        | grep -q 'inet '; then
      return 0
    fi
    sleep 1
  done
  incus info "$(instance "$name")" --project "$(project "$name")" --show-log >&2 || true
  die "yard $name did not become network-ready"
}
status_json() { network status --json; }
status_diagnostic() {
  jq -c '{
    isolation,
    appliedIsolation,
    converged,
    revision,
    appliedRevision,
    pendingStart: (.pendingStart // []),
    removing: (.removing // []),
    bindings: ([(.bindings // [])[].yard.name] | sort),
    links,
    diagnostic: (.diagnostic // "")
  }' <<<"$1" >&2 || true
}
binding() {
  status_json | jq -er --arg name "$1" '.bindings[] | select(.yard.name == $name)'
}
address() { binding "$1" | jq -er '.ipv4'; }
mac() { binding "$1" | jq -er '.mac'; }
guest_address() {
  guest "$1" ip -4 -o address show dev eth0 scope global \
    | awk 'NR == 1 {sub(/\/.*/, "", $4); print $4}'
}
start_tcp_server() {
  local name="$1"
  guest "$name" sh -c \
    'if [ -f /tmp/subyard-network-http.pid ]; then kill "$(cat /tmp/subyard-network-http.pid)" 2>/dev/null || true; fi; nohup python3 -m http.server 18080 --bind 0.0.0.0 >/tmp/subyard-network-http.log 2>&1 </dev/null & echo $! >/tmp/subyard-network-http.pid'
  for _ in $(seq 1 30); do
    guest "$name" timeout 1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/18080' \
      >/dev/null 2>&1 && return 0
    sleep 1
  done
  die "TCP fixture did not start in yard $name"
}
can_connect() {
  guest "$1" timeout 4 bash -c "exec 3<>/dev/tcp/$2/18080" >/dev/null 2>&1
}
assert_blocked() {
  if can_connect "$1" "$2"; then
    die "$1 unexpectedly reached $2"
  fi
}
mark_yard() {
  incus project set "$(project "$1")" user.subyard.e2e="$MARKER"
  incus config set "$(instance "$1")" user.subyard.e2e="$MARKER" \
    --project "$(project "$1")"
}
next_port=$((36000 + ($$ % 9000)))
create_yard_config() {
  local name="$1"
  while ss -Hln "sport = :$next_port" 2>/dev/null | grep -q .; do
    next_port=$((next_port + 1))
  done
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
# $MARKER
SSH_PORT=$next_port
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host/$name
HOST_MOUNTS=
RESTRICTED_DISK_PATHS=$STATE/host/$name
FORWARD_SSH_AGENT=0
EOF
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
  next_port=$((next_port + 1))
}

if [ "$MODE" = resume ]; then
  current_boot="$(cat /proc/sys/kernel/random/boot_id)"
  [ -n "${SUBYARD_E2E_NETWORK_PREBOOT_ID:-}" ] \
    && [ "$current_boot" != "$SUBYARD_E2E_NETWORK_PREBOOT_ID" ] \
    || die "resume did not observe a new VM$SUBYARD_E2E_VM boot ID"
  baseline_bindings="$(<"$STATE/bindings.json")"
  resume_status="$(status_json)"
  if ! jq -e --arg a "${YARDS[0]}" --arg b "${YARDS[1]}" \
    --argjson baseline "$baseline_bindings" '
    .isolation == true and .appliedIsolation == true and .converged == true and
    (.links == [{"a":$a,"b":$b}]) and
    ([.bindings[].yard.name] | sort) == $baseline and
    ((.pendingStart // []) | length == 0)
  ' <<<"$resume_status" >/dev/null; then
    status_diagnostic "$resume_status"
    die 'enabled network policy did not converge across reboot'
  fi
  for name in "${YARDS[@]}"; do wait_guest "$name"; done
  a_ip="$(address "${YARDS[0]}")"
  b_ip="$(address "${YARDS[1]}")"
  c_ip="$(address "${YARDS[2]}")"
  for name in "${YARDS[@]}"; do start_tcp_server "$name"; done
  can_connect "${YARDS[0]}" "$b_ip" || die 'reboot lost the persisted A-B link'
  assert_blocked "${YARDS[0]}" "$c_ip"
  assert_blocked "${YARDS[2]}" "$a_ip"
  ok 'enabled isolation and saved links survived the host reboot'

  info 'tearing down C while isolation is enabled'
  yard "${YARDS[2]}" teardown --yes
  status_json | jq -e --arg c "${YARDS[2]}" '
    .isolation == true and .converged == true and
    ([.bindings[].yard.name] | index($c) | not) and
    ([.links[] | select(.a == $c or .b == $c)] | length == 0)
  ' >/dev/null || die 'isolated teardown left C in the applied policy'

  network isolation off --yes
  yard "${YARDS[0]}" teardown --yes
  yard "${YARDS[1]}" teardown --yes
  status_json | jq -e '
    .isolation == false and .converged == true and
    ((.links // []) | length == 0) and ((.bindings // []) | length == 0)
  ' >/dev/null || die 'final teardown left network bindings or links'
  WORK_COMPLETE=1
  printf 'ok: rebooted network policy and marker-owned teardown converged\n'
  exit 0
fi

for name in "${YARDS[@]:0:2}"; do create_yard_config "$name"; done

info 'building the candidate and creating two normal local yards before isolation'
"$ROOT/dev/build-engine.sh" --force
install -m 0755 "$ROOT/.build/yard" "$YARD_BIN"
status_json | jq -e '
  .isolation == false and .appliedIsolation == false and
  ((.links // []) | length == 0) and ((.bindings // []) | length == 0) and
  ((.pendingStart // []) | length == 0) and ((.removing // []) | length == 0)
' >/dev/null || die 'retained VM has a nonempty network policy'
incus network acl list --project default --format json | jq -e '
  [.[] | select(.config["user.subyard.managed"] == "true")] | length == 0
' >/dev/null || die 'retained VM has a foreign Subyard-managed network ACL'
for name in "${YARDS[@]}"; do
  ! incus project show "$(project "$name")" >/dev/null 2>&1 \
    || die "retained VM already has project $(project "$name")"
done
printf '%s\n' "$MARKER" > "$STATE/.armed"
chmod 0600 "$STATE/.armed"

for name in "${YARDS[@]:0:2}"; do
  yard "$name" init --yes
  yard "$name" start --yes
  mark_yard "$name"
done

for name in "${YARDS[@]:0:2}"; do wait_guest "$name"; done

info 'saving an A-B link before the operator enables host-wide isolation'
network link "${YARDS[0]}" "${YARDS[1]}" --yes
status_json | jq -e --arg a "${YARDS[0]}" --arg b "${YARDS[1]}" '
  .isolation == false and .converged == true and
  (.links == [{"a":$a,"b":$b}])
' >/dev/null || die 'disabled-mode link was not persisted canonically'
network isolation on --yes

info 'creating the third normal yard after host-wide isolation is enabled'
create_yard_config "${YARDS[2]}"
yard "${YARDS[2]}" init --yes
yard "${YARDS[2]}" start --yes
mark_yard "${YARDS[2]}"

post_create_status="$(status_json)"
baseline_bindings="$(jq -cer '[.bindings[].yard.name] | sort' \
  <<<"$post_create_status")"
if ! jq -e --arg a "${YARDS[0]}" --arg b "${YARDS[1]}" \
  --arg c "${YARDS[2]}" --argjson baseline "$baseline_bindings" '
    .isolation == true and .appliedIsolation == true and .converged == true and
    (.links == [{"a":$a,"b":$b}]) and
    ([.bindings[].yard.name] | sort) == $baseline and
    ([$a,$b,$c] - $baseline | length) == 0 and
    ((.pendingStart // []) | length == 0)
  ' <<<"$post_create_status" >/dev/null; then
  status_diagnostic "$post_create_status"
  die 'late-created yard did not join the complete host policy'
fi
baseline_temporary="$(mktemp "$STATE/.bindings.XXXXXX")"
printf '%s\n' "$baseline_bindings" > "$baseline_temporary"
chmod 0600 "$baseline_temporary"
mv -f -- "$baseline_temporary" "$STATE/bindings.json"

for name in "${YARDS[@]}"; do
  wait_guest "$name"
  expected="$(address "$name")"
  expected_mac="$(mac "$name")"
  guest "$name" ip -4 -o address show dev eth0 scope global \
    | awk '{sub(/\/.*/, "", $4); print $4}' | grep -Fxq "$expected" \
    || die "yard $name did not receive its policy-pinned DHCP address"
  incus network list-leases incusbr0 --project "$(project "$name")" --format json \
    | jq -e --arg address "$expected" --arg mac "$expected_mac" '
      .[] | select(
        .address == $address and
        ((.hwaddr | ascii_downcase) == ($mac | ascii_downcase)) and
        ((.type | ascii_downcase) == "static" or (.type | ascii_downcase) == "dynamic")
      )
    ' \
        >/dev/null \
    || die "Incus DHCP lease is missing for yard $name"
  guest "$name" ip -4 route show default | grep -q . \
    || die "DHCP default route is missing in yard $name"
  guest "$name" getent ahostsv4 deb.debian.org >/dev/null \
    || die "DNS failed in yard $name"
  guest "$name" curl -4 -fsS --max-time 15 -o /dev/null https://deb.debian.org/ \
    || die "Internet egress failed in yard $name"
done

gateway="$(incus network get incusbr0 ipv4.address --project default)"
gateway="${gateway%/*}"
for name in "${YARDS[@]}"; do
  guest "$name" timeout 5 bash -c "exec 3<>/dev/tcp/$gateway/22" \
    || die "exact bridge-host TCP/22 exception failed in yard $name"
  timeout --foreground 15 ssh -o BatchMode=yes -o ConnectTimeout=5 \
    "yard-$name" true \
    || die "product SSH proxy failed under isolation for yard $name"
done
ok 'policy preserves DHCP, DNS, Internet egress, and product SSH proxies'

for name in "${YARDS[@]}"; do start_tcp_server "$name"; done
a_ip="$(address "${YARDS[0]}")"
b_ip="$(address "${YARDS[1]}")"
c_ip="$(address "${YARDS[2]}")"
can_connect "${YARDS[0]}" "$b_ip" || die 'A could not reach linked B'
can_connect "${YARDS[1]}" "$a_ip" || die 'B could not reach linked A'
assert_blocked "${YARDS[0]}" "$c_ip"
assert_blocked "${YARDS[2]}" "$a_ip"

info 'adding B-C and proving links are bidirectional but non-transitive'
a_epoch="$(guest "${YARDS[0]}" awk '{print $22}' /proc/1/stat)"
network link "${YARDS[1]}" "${YARDS[2]}" --yes
for name in "${YARDS[@]}"; do wait_guest "$name"; start_tcp_server "$name"; done
[ "$(guest "${YARDS[0]}" awk '{print $22}' /proc/1/stat)" = "$a_epoch" ] \
  || die 'adding B-C restarted unaffected A'
can_connect "${YARDS[0]}" "$b_ip" || die 'A-B link disappeared after adding B-C'
can_connect "${YARDS[1]}" "$c_ip" || die 'B could not reach linked C'
can_connect "${YARDS[2]}" "$b_ip" || die 'C could not reach linked B'
assert_blocked "${YARDS[0]}" "$c_ip"
assert_blocked "${YARDS[2]}" "$a_ip"
ok 'A-B and B-C links do not create an A-C link'

info 'checking IPv4, MAC, and link-local IPv6 bypass resistance'
guest "${YARDS[2]}" ip address add "$b_ip/32" dev eth0
if guest "${YARDS[2]}" timeout 4 python3 -c \
  "import socket;s=socket.socket();s.bind(('$b_ip',0));s.settimeout(3);s.connect(('$a_ip',18080))" \
  >/dev/null 2>&1; then
  guest "${YARDS[2]}" ip address del "$b_ip/32" dev eth0
  die 'C bypassed A isolation with a forged linked IPv4 source'
fi
guest "${YARDS[2]}" ip address del "$b_ip/32" dev eth0

assigned_mac="$(mac "${YARDS[2]}")"
guest "${YARDS[2]}" ip link set eth0 down
guest "${YARDS[2]}" ip link set eth0 address 02:00:00:00:ee:03
guest "${YARDS[2]}" ip link set eth0 up
if guest "${YARDS[2]}" timeout 4 bash -c "exec 3<>/dev/tcp/$gateway/22" \
  >/dev/null 2>&1; then
  die 'yard traffic survived a forged MAC source'
fi
guest "${YARDS[2]}" ip link set eth0 down
guest "${YARDS[2]}" ip link set eth0 address "$assigned_mac"
guest "${YARDS[2]}" ip link set eth0 up
wait_guest "${YARDS[2]}"

b_ipv6="$(guest "${YARDS[1]}" ip -6 -o address show dev eth0 scope link \
  | awk 'NR == 1 {sub(/\/.*/, "", $4); print $4}')"
[ -n "$b_ipv6" ] || die 'B has no link-local IPv6 address to test'
if guest "${YARDS[2]}" ping -6 -c 1 -W 3 "$b_ipv6%eth0" >/dev/null 2>&1; then
  die 'C bypassed isolation over link-local IPv6'
fi
ok 'anti-spoof filters and default ingress drop resist IPv4, MAC, and IPv6 bypasses'

info 'holding a B-C connection while removing that link'
guest "${YARDS[2]}" sh -c \
  'rm -f /tmp/subyard-network-hold-ready; nohup python3 -c '"'"'import socket,time;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(("0.0.0.0",18081));s.listen();open("/tmp/subyard-network-hold-ready","w").write("yes");c,_=s.accept();time.sleep(300)'"'"' >/tmp/subyard-network-hold.log 2>&1 </dev/null &'
for _ in $(seq 1 30); do
  guest "${YARDS[2]}" test -f /tmp/subyard-network-hold-ready >/dev/null 2>&1 && break
  sleep 1
done
guest "${YARDS[2]}" test -f /tmp/subyard-network-hold-ready \
  || die 'B-C hold server did not start'
( guest "${YARDS[1]}" python3 -c \
  "import socket,time;s=socket.socket();s.connect(('$c_ip',18081));open('/tmp/subyard-network-connected','w').write('yes');time.sleep(300)" \
  >"$STATE/established.log" 2>&1 ) &
ESTABLISHED_PID=$!
for _ in $(seq 1 30); do
  guest "${YARDS[1]}" test -f /tmp/subyard-network-connected >/dev/null 2>&1 && break
  sleep 1
done
guest "${YARDS[1]}" test -f /tmp/subyard-network-connected \
  || die 'could not establish the B-C connection before unlink'
b_epoch="$(guest "${YARDS[1]}" awk '{print $22}' /proc/1/stat)"
c_epoch="$(guest "${YARDS[2]}" awk '{print $22}' /proc/1/stat)"
a_epoch="$(guest "${YARDS[0]}" awk '{print $22}' /proc/1/stat)"
network unlink "${YARDS[1]}" "${YARDS[2]}" --yes
for _ in $(seq 1 30); do
  kill -0 "$ESTABLISHED_PID" 2>/dev/null || break
  sleep 1
done
if kill -0 "$ESTABLISHED_PID" 2>/dev/null; then
  die 'established B-C connection survived the stopped unlink transition'
fi
wait "$ESTABLISHED_PID" 2>/dev/null || true
ESTABLISHED_PID=''
for name in "${YARDS[@]}"; do wait_guest "$name"; start_tcp_server "$name"; done
[ "$(guest "${YARDS[1]}" awk '{print $22}' /proc/1/stat)" != "$b_epoch" ] \
  || die 'unlink did not restart B'
[ "$(guest "${YARDS[2]}" awk '{print $22}' /proc/1/stat)" != "$c_epoch" ] \
  || die 'unlink did not restart C'
[ "$(guest "${YARDS[0]}" awk '{print $22}' /proc/1/stat)" = "$a_epoch" ] \
  || die 'unlinking B-C restarted unaffected A'
assert_blocked "${YARDS[1]}" "$c_ip"
assert_blocked "${YARDS[2]}" "$b_ip"
can_connect "${YARDS[0]}" "$b_ip" || die 'unlinking B-C disturbed A-B'
ok 'unlink stops affected yards, closes established flows, and blocks new flows'

info 'disabling and re-enabling isolation while retaining the saved A-B link'
network isolation off --yes
for name in "${YARDS[@]}"; do wait_guest "$name"; start_tcp_server "$name"; done
c_ip="$(guest_address "${YARDS[2]}")"
can_connect "${YARDS[0]}" "$c_ip" || die 'disabled isolation still blocked A-C'
status_json | jq -e --arg a "${YARDS[0]}" --arg b "${YARDS[1]}" '
  .isolation == false and .appliedIsolation == false and .converged == true and
  (.bindings | length == 0) and (.links == [{"a":$a,"b":$b}])
' >/dev/null || die 'disabled policy did not retain only A-B'
network isolation on --yes
for name in "${YARDS[@]}"; do wait_guest "$name"; start_tcp_server "$name"; done
b_ip="$(address "${YARDS[1]}")"
c_ip="$(address "${YARDS[2]}")"
can_connect "${YARDS[0]}" "$b_ip" || die 're-enabled isolation lost saved A-B'
assert_blocked "${YARDS[0]}" "$c_ip"
ok 'isolation off/on preserves explicit links and restores their enforcement'

info 'reconciling, stopping, and starting a normal yard while isolation is enabled'
yard "${YARDS[0]}" init --yes
yard "${YARDS[0]}" stop --yes
yard "${YARDS[0]}" start --yes
wait_guest "${YARDS[0]}"
start_tcp_server "${YARDS[1]}"
can_connect "${YARDS[0]}" "$b_ip" \
  || die 'normal init/stop/start lost the isolated A-B link'

info 'resetting and recreating A while isolation remains enabled'
yard "${YARDS[0]}" stop --yes
yard "${YARDS[0]}" init --reset --yes
mark_yard "${YARDS[0]}"
yard "${YARDS[0]}" start --yes
wait_guest "${YARDS[0]}"
a_ip="$(address "${YARDS[0]}")"
b_ip="$(address "${YARDS[1]}")"
start_tcp_server "${YARDS[0]}"
start_tcp_server "${YARDS[1]}"
can_connect "${YARDS[0]}" "$b_ip" \
  || die 'reset/recreate lost the isolated A-B link'
can_connect "${YARDS[1]}" "$a_ip" \
  || die 'reset/recreate lost the reverse B-A link'
reset_status="$(status_json)"
baseline_bindings="$(<"$STATE/bindings.json")"
if ! jq -e --argjson baseline "$baseline_bindings" '
  .isolation == true and .appliedIsolation == true and .converged == true and
  ([.bindings[].yard.name] | sort) == $baseline and
  ((.pendingStart // []) | length == 0)
' <<<"$reset_status" >/dev/null; then
  status_diagnostic "$reset_status"
  die 'reset/recreate left the network policy unconverged'
fi

WORK_COMPLETE=1
printf 'ok: explicit links, opt-in isolation, late yard creation, and reset are ready for reboot\n'
