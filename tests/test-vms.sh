#!/usr/bin/env bash
# Host-free contracts for the retained L1 physical provisioner.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
PROVISION="$ROOT/scripts/e2e-lab/provision.sh"

source_guard="$(sed -n '/^\[ "${BASH_SOURCE\[0\]:-\$0}" = "\$0" \] || return 0$/p' "$PROVISION")"
[ -n "$source_guard" ] || fail "inner provisioner has no stdin-safe source guard"
printf '%s\n' "$source_guard" | bash -u -s \
  || fail "inner provisioner source guard rejected bash -s execution"
! grep -Eq 'usermod[[:space:]]+-aG[[:space:]]+(incus-admin|yard)' "$PROVISION" \
  || fail "inner provisioner grants dev access to privileged inner groups"
grep -Fq 'iifname "e2e-vm-net-*" drop' "$PROVISION" \
  && grep -Fq 'iifname "e2e-vm-net-*" oifname "e2e-vm-net-*" drop' "$PROVISION" \
  || fail "inner provisioner does not block guest-initiated access to L1"
grep -Fq 'PasswordAuthentication no' "$PROVISION" \
  || fail "bastion provisioner does not disable password authentication"
grep -Fq 'ssh-keygen -A' "$ROOT/internal/adapters/testvmsruntime/guest.go" \
  || fail "guest SSH reconciliation does not repair missing host keys"
grep -Fq 'AuthorizedKeysCommand /usr/local/libexec/subyard/test-vms-authorized-key %u %t %k' "$PROVISION" \
  && grep -Fq 'restrict,command="sudo -n /usr/local/libexec/subyard/test-vms-inner _test-vms-facade"' "$PROVISION" \
  && grep -Fq 'NOPASSWD: /usr/local/libexec/subyard/test-vms-inner _test-vms-facade' "$PROVISION" \
  || fail "default-open controller admission is not bounded by the forced facade"
! sed -n '/^reconcile_agent_sshd_policy()/,/^}/p' "$PROVISION" \
  | grep -Fq 'rm -f /usr/local/libexec/subyard/test-vms-authorized-key' \
  || fail "controller AuthorizedKeysCommand helper is deleted during reconciliation"
grep -Fq 'subyard-e2e-slot-' "$PROVISION" \
  || fail "physical provisioner does not isolate slot data accounts"
grep -Fq 'printf '\''test-vms-v1\n'\'' > "$home/.subyard-managed"' "$PROVISION" \
  || fail "slot data-account homes have no exact managed marker"
grep -Fq 'existing slot account %s is not Subyard-shaped' "$PROVISION" \
  && grep -Fq 'existing slot home %s is not Subyard-managed' "$PROVISION" \
  && grep -Fq 'legacy slot home %s is not safely recognizable' "$PROVISION" \
  || fail "slot data-account reconciliation may adopt a foreign account or home"
grep -Fq 'slot_home_has_managed_marker' "$PROVISION" \
  && grep -Fq 'legacy_slot_home_is_recognizable' "$PROVISION" \
  && grep -Fq '[ -e "$home/.subyard-managed" ] || [ -L "$home/.subyard-managed" ]' "$PROVISION" \
  && grep -Fq '[ ! -s "$keys" ]' "$PROVISION" \
  && grep -Fq '[ "$ssh_gid" = "$keys_gid" ] && [ "$ssh_gid" -ne 0 ]' "$PROVISION" \
  || fail "slot data-account ownership markers are not validated fail-closed"
grep -Fq 'apt-get install -y -qq --no-install-recommends' "$PROVISION" \
  || fail "inner VM backend installs optional QEMU desktop packages"
grep -Fq 'run_with_progress "installing inner Incus and QEMU"' "$PROVISION" \
  && grep -Fq 'still working, %ss elapsed' "$PROVISION" \
  || fail "inner VM backend package installation has no periodic progress"
grep -Fq 'install -d -m 0750 -o root -g "$primary" "$home/.ssh"' "$PROVISION" \
  || fail "bastion SSH directory is not root-owned and account-readable"
grep -Fq 'candidate="$(mktemp "$home/.ssh/.authorized_keys.XXXXXX")"' "$PROVISION" \
  && grep -Fq 'chown root:"$primary" "$candidate"' "$PROVISION" \
  && grep -Fq 'mv -fT -- "$candidate" "$keys"' "$PROVISION" \
  || fail "bastion static authorized_keys is not atomically retired"
grep -Fq '[ -f "$keys" ] && [ ! -L "$keys" ]' "$PROVISION" \
  && grep -Fq 'stat -c '\''%u:%h'\''' "$PROVISION" \
  && grep -Fq 'agent authorized_keys is unsafe' "$PROVISION" \
  || fail "bastion authorized_keys replacement is not fail-closed"
policy_call="$(grep -n '^reconcile_agent_sshd_policy$' "$PROVISION" | tail -n1 | cut -d: -f1)"
retire_call="$(grep -n '^retire_agent_static_keys$' "$PROVISION" | tail -n1 | cut -d: -f1)"
[ -n "$policy_call" ] && [ -n "$retire_call" ] && [ "$policy_call" -lt "$retire_call" ] \
  || fail "static controller key is retired before dynamic admission is active"
grep -Fq '_test-vms-worker gc' "$PROVISION" \
  && grep -Fq '_test-vms-worker reconcile-pool --yes' "$PROVISION" \
  || fail "physical provisioner does not invoke the installed Go worker"
grep -Fq 'config_candidate="$(mktemp /etc/subyard/.test-vms.env.XXXXXX)"' "$PROVISION" \
  && grep -Fq 'SUBYARD_TEST_VMS_CONFIG="$config_candidate"' "$PROVISION" \
  && grep -Fq 'mv -f -- "$config_candidate" /etc/subyard/test-vms.env' "$PROVISION" \
  && ! grep -Fq '} > /etc/subyard/test-vms.env' "$PROVISION" \
  || fail "test-vms config is published before pool reconcile succeeds"
grep -Fq 'subyard-test-vms-lease-reaper.timer' "$PROVISION" \
  && grep -Fq '_test-vms-worker broker-start' "$PROVISION" \
  && grep -Fq '_test-vms-worker drain-all' "$PROVISION" \
  && ! grep -Fq 'Remove expired Subyard disposable test VMs' "$PROVISION" \
  || fail "physical provisioner did not replace allocation-age GC with lease reaping"
grep -Fq 'E2E_BROKER_SOURCE' "$PROVISION" \
  && grep -Fq 'install-test-vms-host-sink.sh' "$ROOT/internal/adapters/reconcileruntime/runtime.go" \
  && grep -Fq 'systemctl enable --now "$(basename "$TIMER_PATH")"' \
    "$ROOT/scripts/install-test-vms-host-sink.sh" \
  && ! grep -Fq 'systemctl start "$(basename "$SERVICE_PATH")"' \
    "$ROOT/scripts/install-test-vms-host-sink.sh" \
  || fail "broker spool producer and host sink are not rolled out sink-first"

mkdir -p "$TMP/invoke-bin"
cat > "$TMP/invoke-bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
joined="$*"
case "$joined" in
  *' test -x /usr/local/libexec/subyard/test-vms-inner') exit 0 ;;
  *' sh -c '*)
    [ "${TEST_VMS_WORKER_KIND:?}" = go ]
    exit
    ;;
esac
printf '%s\n' "$joined" > "$TEST_VMS_INVOKE_LOG"
SH
chmod 0700 "$TMP/invoke-bin/incus"
for worker_kind in legacy go; do
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$TMP/invoke-$worker_kind"
    export PATH="$TMP/invoke-bin:$PATH"
    export TEST_VMS_WORKER_KIND="$worker_kind"
    export TEST_VMS_INVOKE_LOG="$TMP/invoke-$worker_kind.log"
    bash "$ROOT/scripts/e2e-lab/invoke.sh" status
  )
done
grep -Fq '/usr/local/libexec/subyard/test-vms-inner status' "$TMP/invoke-legacy.log" \
  || fail "current invoke cannot inspect a legacy Shell worker"
grep -Fq '/usr/local/libexec/subyard/test-vms-inner _test-vms-worker status' "$TMP/invoke-go.log" \
  || fail "current invoke omitted the Go worker entrypoint"

if SUBYARD_E2E_LEGACY_FIXTURE=1 \
  bash "$ROOT/dev/e2e/seed-test-vms-legacy-state.sh" fixture-project fixture-instance \
  >/dev/null 2>&1; then
  fail "legacy upgrade fixture ran outside disposable VM1"
fi
grep -Fq 'user.subyard.managed' "$ROOT/dev/e2e/seed-test-vms-legacy-state.sh" \
  || fail "legacy upgrade fixture does not verify candidate ownership"
grep -Fq 'subyard-managed-e2e-agent' "$ROOT/dev/e2e/seed-test-vms-legacy-state.sh" \
  && grep -Fq 'legacy static controller key survived' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "legacy upgrade fixture does not exercise static controller-key retirement"

# shellcheck source=scripts/e2e-lab/provision.sh
. "$PROVISION"

E2E_VM_STATE_DIR="$TMP/legacy-test-vms-state"
mkdir -p "$E2E_VM_STATE_DIR"
chmod 2770 "$E2E_VM_STATE_DIR"
printf 'legacy\n' > "$E2E_VM_STATE_DIR/worker-key"
chmod 0660 "$E2E_VM_STATE_DIR/worker-key"
reconcile_test_vm_state_directory
[ "$(stat -c '%a' "$E2E_VM_STATE_DIR")" = 700 ] \
  || fail "legacy test-vms state directory retained its setgid boundary"
[ "$(stat -c '%a' "$E2E_VM_STATE_DIR/worker-key")" = 600 ] \
  || fail "legacy test-vms state file retained group access"

export SUBYARD_INNER_INCUS_APPARMOR_DROPIN="$TMP/incus.service.d/subyard-nested-e2e.conf"
systemctl() {
  printf '%s\n' "$*" >> "$TMP/systemctl-calls"
  case "$*" in 'is-active --quiet incus.service') return 0 ;; esac
}
reconcile_inner_apparmor_compat
reconcile_inner_apparmor_compat
grep -Fxq 'Environment=INCUS_SECURITY_APPARMOR=false' "$SUBYARD_INNER_INCUS_APPARMOR_DROPIN" \
  || fail "inner Incus AppArmor compatibility drop-in was not installed"
grep -Fxq 'TimeoutStartSec=45s' "$SUBYARD_INNER_INCUS_APPARMOR_DROPIN" \
  || fail "inner Incus start can wait for its ten-minute default"
[ "$(grep -c '^restart incus.service$' "$TMP/systemctl-calls")" -eq 1 ] \
  || fail "idempotent AppArmor reconciliation restarted Incus more than once"
unset -f systemctl

rm -f "$SUBYARD_INNER_INCUS_APPARMOR_DROPIN"
: > "$TMP/systemctl-calls"
: > "$TMP/systemctl-active-calls"
sleep() { :; }
systemctl() {
  local active_calls
  printf '%s\n' "$*" >> "$TMP/systemctl-calls"
  case "$*" in
    'restart incus.service') return 1 ;;
    'is-active --quiet incus.service')
      active_calls="$(wc -l < "$TMP/systemctl-active-calls")"
      printf '.\n' >> "$TMP/systemctl-active-calls"
      [ "$active_calls" -eq 0 ] && return 0
      [ "$active_calls" -ge 31 ]
      ;;
    'start incus.service') return 0 ;;
  esac
}
reconcile_inner_apparmor_compat
[ "$(grep -c '^restart incus.service$' "$TMP/systemctl-calls")" -eq 1 ] \
  && [ "$(grep -c '^start incus.service$' "$TMP/systemctl-calls")" -eq 1 ] \
  && [ "$(grep -c '^reset-failed incus.service$' "$TMP/systemctl-calls")" -eq 1 ] \
  || fail "inner Incus transient restart was not retried through bounded start"
unset -f systemctl sleep

INNER_FIXTURE="$TMP/inner-incus"
mkdir -p "$INNER_FIXTURE"
install() {
  [ "${*: -1}" = /srv/incus-e2e/storage ] || command install "$@"
}
incus() {
  local leaked
  if IFS= read -r leaked; then
    printf '%s\n' "$leaked" > "$INNER_FIXTURE/consumed-stdin"
    return 65
  fi
  printf '%s\n' "$*" >> "$INNER_FIXTURE/calls"
  case "$*" in
    'storage show default --project default') [ -f "$INNER_FIXTURE/storage" ] ;;
    'storage create default dir source=/srv/incus-e2e/storage --project default')
      : > "$INNER_FIXTURE/storage" ;;
    'network show incusbr0 --project default') [ -f "$INNER_FIXTURE/network" ] ;;
    'network create incusbr0 ipv4.address=auto ipv6.address=none --project default')
      printf 'dnsmasq: cannot open log : Permission denied\n'; return 3 ;;
    'network create incusbr0 ipv4.address=auto ipv6.address=none raw.dnsmasq=log-facility=/var/lib/incus/networks/incusbr0/dnsmasq.log --project default')
      : > "$INNER_FIXTURE/network" ;;
    'profile show default --project default') [ -f "$INNER_FIXTURE/profile" ] ;;
    'profile create default --project default') : > "$INNER_FIXTURE/profile" ;;
    'profile device list default --project default')
      [ -f "$INNER_FIXTURE/root" ] && printf 'root\n'
      [ -f "$INNER_FIXTURE/eth0" ] && printf 'eth0\n'
      return 0
      ;;
    'profile device add default root disk pool=default path=/ --project default')
      : > "$INNER_FIXTURE/root" ;;
    'profile device add default eth0 nic network=incusbr0 --project default')
      : > "$INNER_FIXTURE/eth0" ;;
    *) return 64 ;;
  esac
}
printf 'cat > /etc/systemd/system/subyard-test-vms-gc.service\n' | reconcile_inner_incus
reconcile_inner_incus
[ ! -e "$INNER_FIXTURE/consumed-stdin" ] \
  || fail "inner Incus CLI consumed the remaining streamed provisioner"
[ "$(grep -c '^storage create ' "$INNER_FIXTURE/calls")" -eq 1 ] \
  || fail "inner storage bootstrap was not idempotent"
[ "$(grep -c '^network create ' "$INNER_FIXTURE/calls")" -eq 2 ] \
  || fail "inner network bootstrap did not perform one file-log fallback"
[ "$(grep -c '^profile device add ' "$INNER_FIXTURE/calls")" -eq 2 ] \
  || fail "inner default profile bootstrap was not idempotent"

printf 'ok: test-vms physical bootstrap is bounded and idempotent\n'
