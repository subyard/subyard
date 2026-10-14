#!/usr/bin/env bash
# Host-free contracts for the L1 physical provisioner.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-test-vms.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
PROVISION="$ROOT/scripts/e2e-lab/provision.sh"

# Never invoke a function being tested in an if/! condition: Bash would suppress errexit
# throughout its body and hide failed intermediate commands.
expect_failure() {
  local rc
  set +e
  ( set -e; "$@" ) > "$TMP/refusal" 2>&1
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || fail "accepted: $*"
}
(
  id() { fail 'sourcing queried the host account'; }
  install() { fail 'sourcing installed files'; }
  systemctl() { fail 'sourcing changed services'; }
  # shellcheck source=scripts/e2e-lab/provision.sh
  . "$PROVISION"
  declare -F prepare_test_vm_config >/dev/null
)
(
  subyard_download_https_atomic() { fail 'streamed execution downloaded before root guard'; }
  id() { printf '1000\n'; }
  export -f subyard_download_https_atomic id
  expect_failure bash -u -s < "$PROVISION"
  grep -Fxq 'test-vms provision requires root inside the yard' "$TMP/refusal" \
    || fail 'streamed provisioner did not reach its root guard'
)
# shellcheck source=scripts/e2e-lab/provision.sh
. "$PROVISION"

# The package function calls the real progress helper and bounded native command fixtures.
(
  package_calls="$TMP/package-calls"
  apt-get() { printf '%s\n' "$*" >> "$package_calls"; }
  version_ok() { return 0; }
  mkdir "$TMP/metadata-bin"
  printf '#!/bin/sh\nexit 0\n' > "$TMP/metadata-bin/mkisofs"
  chmod 0700 "$TMP/metadata-bin/mkisofs"
  PATH="$TMP/metadata-bin:$PATH" reconcile_inner_packages
  cat > "$TMP/package-expected" <<'EOF'
update -qq
install -y -qq --no-install-recommends incus qemu-system-x86 qemu-utils ovmf openssh-client nftables genisoimage
EOF
  cmp "$TMP/package-expected" "$package_calls" || fail 'package requests changed'
  # Isolate command lookup from any system mkisofs, leaving only the tested shell functions.
  sleep() { return 1; }
  expect_failure env PATH="$TMP/missing" /bin/bash -c '
    subyard_download_https_atomic() { :; }
    . "$1"
    apt-get() { :; }; version_ok() { return 0; }; sleep() { return 1; }
    reconcile_inner_packages
  ' bash "$PROVISION"
  grep -Fxq 'mkisofs is required for VM cloud-init metadata disks' "$TMP/refusal" \
    || fail 'missing metadata prerequisite diagnostic'
)
(
  sleep() { command sleep 0.02; }
  printf() {
    command printf "$@"
    case "$1" in *'still working,'*) touch "$TMP/progress-seen";; esac
  }
  run_with_progress 'fixture success' timeout 5 bash -c '
    until [ -e "$1" ]; do sleep 0.01; done
  ' bash "$TMP/progress-seen" > "$TMP/progress"
  set +e
  run_with_progress 'fixture failure' bash -c 'exit 23' >> "$TMP/progress"
  rc=$?
  set -e
  [ "$rc" = 23 ] || fail 'progress helper lost command failure status'
  grep -Fq 'fixture success (still working,' "$TMP/progress" \
    || fail 'progress helper did not report a slow operation'
  [ -z "$(jobs -pr)" ] || fail 'progress helper left its ticker running'
)

budget_fixture=(NESTED_E2E_VMS=1 DEV_USER=dev E2E_DISK_BUDGET=120GiB
  E2E_CACHE_BUDGET=20GiB E2E_DISK_RESERVE=6GiB E2E_MEMORY_RESERVE=3GiB E2E_VM_OVERHEAD=768MiB
  E2E_VM_IMAGE=images:debian/13/cloud E2E_VM_CPU=4 E2E_VM_MEMORY=4GiB E2E_VM_DISK=20GiB
  E2E_VM_SLOT_COUNT=2 E2E_VM_BOOT_TIMEOUT=300 E2E_VM_STATE_DIR=/var/lib/subyard/test-vms
  E2E_BROKER_SOURCE=test-yard E2E_AGENT_USER=subyard-e2e-agent E2E_AGENT_HOME=/var/lib/subyard/e2e-agent
  E2E_AGENT_PUBLIC_KEY=)
config_command='. "$1"; prepare_test_vm_config "$2"; cat "$config_candidate"'
env "${budget_fixture[@]}" bash -c "$config_command" bash "$PROVISION" "$TMP/config" > "$TMP/config-output"
cat > "$TMP/config-expected" <<'EOF'
NESTED_E2E_VMS=1
DEV_USER=dev
E2E_DISK_BUDGET=120GiB
E2E_CACHE_BUDGET=20GiB
E2E_DISK_RESERVE=6GiB
E2E_MEMORY_RESERVE=3GiB
E2E_VM_OVERHEAD=768MiB
E2E_RECIPE_ROOT=/usr/local/libexec/subyard/e2e-recipes
E2E_VM_IMAGE=images:debian/13/cloud
E2E_VM_CPU=4
E2E_VM_MEMORY=4GiB
E2E_VM_DISK=20GiB
E2E_VM_SLOT_COUNT=2
E2E_VM_BOOT_TIMEOUT=300
E2E_VM_STATE_DIR=/var/lib/subyard/test-vms
E2E_BROKER_SOURCE=test-yard
E2E_AGENT_USER=subyard-e2e-agent
E2E_AGENT_HOME=/var/lib/subyard/e2e-agent
E2E_AGENT_PUBLIC_KEY=''
EOF
cmp "$TMP/config-expected" "$TMP/config-output" || fail 'config serialization changed'
env "${budget_fixture[@]}" E2E_DISK_BUDGET=0GiB bash -c "$config_command" \
  bash "$PROVISION" "$TMP/config" > "$TMP/unlimited-config"
grep -Fxq E2E_DISK_BUDGET=0GiB "$TMP/unlimited-config" || fail 'unlimited disk budget was lost'
for budget_name in E2E_CACHE_BUDGET E2E_DISK_RESERVE E2E_MEMORY_RESERVE E2E_VM_OVERHEAD; do
  expect_failure env "${budget_fixture[@]}" "$budget_name=0GiB" bash -c "$config_command" bash "$PROVISION" "$TMP/config"
  grep -Fxq "invalid $budget_name" "$TMP/refusal" || fail "wrong zero-budget refusal: $budget_name"
done
for budget_name in E2E_DISK_BUDGET E2E_CACHE_BUDGET E2E_DISK_RESERVE E2E_MEMORY_RESERVE E2E_VM_OVERHEAD; do
  expect_failure env "${budget_fixture[@]}" "$budget_name=" bash -c "$config_command" bash "$PROVISION" "$TMP/config"
  grep -Fxq "missing $budget_name" "$TMP/refusal" || fail "wrong missing-budget refusal: $budget_name"
done

# Verify the installed workers use the candidate while the published config is still old.
mkdir "$TMP/broker-units"
cat > "$TMP/worker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$*" = '_test-vms-worker reconcile-pool --yes' ]
[ "$(cat "$PUBLISHED_CONFIG")" = old-config ]
[ "$SUBYARD_TEST_VMS_CONFIG" != "$PUBLISHED_CONFIG" ]
cmp "$EXPECTED_CONFIG" "$SUBYARD_TEST_VMS_CONFIG"
printf 'reconcile\n' >> "$BROKER_CALLS"
exit "$RECONCILE_RC"
EOF
chmod 0700 "$TMP/worker"
for reconcile_rc in 19 0; do
  printf 'old-config\n' > "$TMP/config/test-vms.env"
  : > "$TMP/broker-calls"
  set +e
  env "${budget_fixture[@]}" RECONCILE_RC="$reconcile_rc" PUBLISHED_CONFIG="$TMP/config/test-vms.env" \
    EXPECTED_CONFIG="$TMP/config-expected" BROKER_CALLS="$TMP/broker-calls" \
    bash -c '
      . "$1"
      systemctl() { printf "%s\n" "$*" >> "$BROKER_CALLS"; }
      prepare_test_vm_config "$2"
      reconcile_test_vm_broker "$3" "$2/test-vms.env" "$4"
    ' bash "$PROVISION" "$TMP/config" "$TMP/broker-units" "$TMP/worker"
  rc=$?
  set -e
  [ "$rc" = "$reconcile_rc" ] || fail 'broker reconciliation lost its failure status'
  [ -z "$(find "$TMP/config" -name '.test-vms.env.*' -print)" ] || fail 'config candidate leaked'
  if [ "$reconcile_rc" = 19 ]; then
    [ "$(cat "$TMP/config/test-vms.env")" = old-config ] || fail 'failed pool reconciliation published config'
    ! grep -q '^enable ' "$TMP/broker-calls" || fail 'failed pool reconciliation enabled broker'
  else
    cmp "$TMP/config-expected" "$TMP/config/test-vms.env" || fail 'successful pool reconciliation lost config'
    [ "$(stat -c '%a' "$TMP/config/test-vms.env")" = 644 ] || fail 'config mode changed'
    cat > "$TMP/broker-expected" <<'EOF'
disable --now subyard-test-vms-gc.timer
daemon-reload
start subyardtestvms.slice
set-property --runtime subyardtestvms.slice MemoryMax=2G MemorySwapMax=2G
reconcile
enable --now subyard-test-vms-lease-reaper.timer
enable --now subyard-test-vms-broker.service
EOF
    cmp "$TMP/broker-expected" "$TMP/broker-calls" || fail 'broker startup order changed'
  fi
done
for expected in 'MemoryMax=2G' 'MemorySwapMax=2G'; do
  grep -Fxq "$expected" "$TMP/broker-units/subyardtestvms.slice" || fail "missing worker limit: $expected"
done
grep -Fxq 'LoadCredential=subyard-broker-request:/run/subyard-test-vms-workers/%N.json' \
  "$TMP/broker-units/subyard-test-vms-worker-.service.d/zzzz-subyard-credentials.conf" || fail 'worker credential binding lost'
grep -Fxq 'ExecStart=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker gc' \
  "$TMP/broker-units/subyard-test-vms-lease-reaper.service" || fail 'reaper lost worker entrypoint'
for expected in 'OnBootSec=1min' 'OnUnitActiveSec=1min' 'Persistent=true'; do
  grep -Fxq "$expected" "$TMP/broker-units/subyard-test-vms-lease-reaper.timer" || fail "missing reaper timing: $expected"
done
for expected in 'ExecStart=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker broker-start' \
  'ExecStart=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker gc' \
  'ExecStop=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker drain-all'; do
  grep -Fxq "$expected" "$TMP/broker-units/subyard-test-vms-broker.service" || fail "missing broker lifecycle: $expected"
done

(
  mkdir "$TMP/policy"
  export E2E_AGENT_USER=subyard-e2e-agent
  visudo() { [ "$*" = "-cf $TMP/policy/sudoers" ]; }
  sshd() { [ "$*" = -t ]; }
  systemctl() { [ "$*" = 'reload ssh.service' ]; }
  reconcile_agent_sshd_policy "$TMP/policy/helper" "$TMP/policy/sudoers" "$TMP/policy/ssh.conf"
  expected='restrict,command="sudo -n /usr/local/libexec/subyard/test-vms-inner _test-vms-facade" ssh-ed25519 AAAA+/=='
  [ "$("$TMP/policy/helper" subyard-e2e-agent ssh-ed25519 AAAA+/==)" = "$expected" ] || fail 'controller helper lost forced facade'
  for arguments in 'dev ssh-ed25519 AAAA' 'subyard-e2e-agent ssh-rsa AAAA' \
    'subyard-e2e-agent ssh-ed25519 bad;blob' 'subyard-e2e-agent ssh-ed25519'; do
    read -r -a helper_arguments <<< "$arguments"
    [ -z "$("$TMP/policy/helper" "${helper_arguments[@]}")" ] || fail 'controller helper admitted invalid key'
  done
  grep -Fxq 'subyard-e2e-agent ALL=(root) NOPASSWD: /usr/local/libexec/subyard/test-vms-inner _test-vms-facade' "$TMP/policy/sudoers" || fail 'facade sudo admission changed'
  for expected in 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' '    AuthorizedKeysFile none' \
    '    AuthorizedKeysCommand /usr/local/libexec/subyard/test-vms-authorized-key %u %t %k' \
    '    AuthorizedKeysCommandUser nobody'; do
    grep -Fxq "$expected" "$TMP/policy/ssh.conf" || fail "SSH policy lost: $expected"
  done
  [ "$(stat -c '%a' "$TMP/policy/helper")" = 755 ] \
    && [ "$(stat -c '%a' "$TMP/policy/sudoers")" = 440 ] \
    && [ "$(stat -c '%a' "$TMP/policy/ssh.conf")" = 644 ] || fail 'controller policy modes changed'
  # Root can replace a 0440 file; the ownership fixture runs as the unprivileged test user.
  chmod u+w "$TMP/policy/sudoers"
  reconcile_agent_sshd_policy "$TMP/policy/helper" "$TMP/policy/sudoers" "$TMP/policy/ssh.conf"
  [ -x "$TMP/policy/helper" ] || fail 'controller helper disappeared on reconciliation'
)
(
  DEV_USER=dev
  getent() { [ "$1" = group ]; }
  id() { case "$1" in -u) printf '1000\n';; -nG) printf 'dev incus-admin yard\n';; esac; }
  gpasswd() { printf '%s\n' "$*" >> "$TMP/access-calls"; }
  usermod() { printf 'usermod %s\n' "$*" >> "$TMP/access-calls"; }
  groupadd() { printf 'groupadd %s\n' "$*" >> "$TMP/access-calls"; }
  reconcile_test_vm_state_directory() { :; }
  reconcile_agent_account() { :; }
  reconcile_slot_accounts() { :; }
  reconcile_agent_sshd_policy() { printf 'policy\n' >> "$TMP/access-calls"; return "${POLICY_RC:-0}"; }
  retire_agent_static_keys() { printf 'retire\n' >> "$TMP/access-calls"; }
  reconcile_test_vm_access
  [ "$(cat "$TMP/access-calls")" = $'-d dev incus-admin\n-d dev yard\npolicy\nretire' ] || fail 'controller migration order or privileged groups changed'
  : > "$TMP/access-calls"
  POLICY_RC=1 expect_failure reconcile_test_vm_access
  ! grep -q '^retire$' "$TMP/access-calls" || fail 'static key retired after policy failure'
)

# Model ownership only; file types, modes, link counts and replacement are native filesystem checks.
account_fixture() {
  fixture_primary="$(command id -gn)"
  id() { case "$1" in -u) printf '1001\n';; -gn|-nG) printf '%s\n' "$fixture_primary";; esac; }
  usermod() { printf '%s\n' "$*" >> "$TMP/account-calls"; }
  useradd() { printf '%s\n' "$*" >> "$TMP/account-calls"; }
  gpasswd() { printf '%s\n' "$*" >> "$TMP/account-calls"; }
  chown() { printf '%s\n' "$*" >> "$TMP/account-calls"; }
  install() {
    printf 'install %s\n' "$*" >> "$TMP/account-calls"
    local -a arguments=()
    while [ "$#" -gt 0 ]; do
      case "$1" in -o|-g) shift 2;; *) arguments+=("$1"); shift;; esac
    done
    command install "${arguments[@]}"
  }
  stat() {
    local mode links target="${!#}"
    mode="$(command stat -c '%a' "$target")"
    links="$(command stat -c '%h' "$target")"
    case "$2" in
      %u:%h) printf '%s:%s\n' "${FIXTURE_UID:-0}" "$links";;
      %U:%G:%a:%h) printf '%s:%s:%s:%s\n' "${FIXTURE_OWNER:-root}" "$fixture_primary" "$mode" "$links";;
      %u:%g:%a) printf '%s:%s:%s\n' "${FIXTURE_UID:-0}" "${FIXTURE_GID:-0}" "$mode";;
      %u:%a) printf '%s:%s\n' "${FIXTURE_UID:-0}" "$mode";;
      %g)
        if [[ "$target" = */authorized_keys ]]; then
          printf '%s\n' "${FIXTURE_KEYS_GID:-${FIXTURE_SSH_GID:-1001}}"
        else printf '%s\n' "${FIXTURE_SSH_GID:-1001}"; fi;;
      *) command stat "$@";;
    esac
  }
}
(
  account_fixture
  E2E_AGENT_USER=subyard-e2e-agent E2E_AGENT_HOME="$TMP/account"
  reconcile_agent_account
  grep -Fxq "install -d -m 0750 -o root -g $fixture_primary $E2E_AGENT_HOME/.ssh" "$TMP/account-calls" || fail 'SSH directory lost root ownership'
  printf 'legacy static key\n' > "$E2E_AGENT_HOME/.ssh/authorized_keys"
  old_inode="$(command stat -c '%i' "$E2E_AGENT_HOME/.ssh/authorized_keys")"
  retire_agent_static_keys
  grep -Fq "root:$fixture_primary $E2E_AGENT_HOME/.ssh/.authorized_keys." "$TMP/account-calls" || fail 'retirement candidate lost root ownership'
  [ ! -s "$E2E_AGENT_HOME/.ssh/authorized_keys" ] \
    && [ "$(command stat -c '%i' "$E2E_AGENT_HOME/.ssh/authorized_keys")" != "$old_inode" ] || fail 'static key was not atomically retired'
  [ "$(command stat -c '%a' "$E2E_AGENT_HOME/.ssh/authorized_keys")" = 640 ] || fail 'retired keys lost protected mode'
  printf 'legacy static key\n' > "$E2E_AGENT_HOME/.ssh/authorized_keys"
  mv() { return 17; }
  expect_failure retire_agent_static_keys
  [ "$(cat "$E2E_AGENT_HOME/.ssh/authorized_keys")" = 'legacy static key' ] || fail 'failed key rename changed admission'
  [ -z "$(find "$E2E_AGENT_HOME/.ssh" -name '.authorized_keys.*' -print)" ] || fail 'failed key rename leaked candidate'
  unset -f mv
  FIXTURE_OWNER=foreign expect_failure retire_agent_static_keys
  FIXTURE_UID=1001 expect_failure reconcile_agent_account
  ln "$E2E_AGENT_HOME/.ssh/authorized_keys" "$TMP/key-hardlink"
  expect_failure reconcile_agent_account
  expect_failure retire_agent_static_keys
  rm "$TMP/key-hardlink" "$E2E_AGENT_HOME/.ssh/authorized_keys"
  ln -s "$TMP/config-output" "$E2E_AGENT_HOME/.ssh/authorized_keys"
  expect_failure reconcile_agent_account
  expect_failure retire_agent_static_keys
  rm "$E2E_AGENT_HOME/.ssh/authorized_keys"
  mkdir "$E2E_AGENT_HOME/.ssh/authorized_keys"
  expect_failure reconcile_agent_account
  rm -r "$E2E_AGENT_HOME/.ssh"
  ln -s "$TMP/policy" "$E2E_AGENT_HOME/.ssh"
  expect_failure reconcile_agent_account
)
(
  account_fixture
  E2E_VM_SLOT_COUNT=1
  homes="$TMP/slots" home="$TMP/slots/1"
  getent() { printf 'subyard-e2e-slot-1:x:1001:1001::%s:%s\n' "${SLOT_ACCOUNT_HOME:-$home}" "${SLOT_ACCOUNT_SHELL:-/bin/sh}"; }
  mkdir -p "$home/.ssh"
  chmod 0755 "$home"; chmod 0750 "$home/.ssh"
  : > "$home/.ssh/authorized_keys"; chmod 0640 "$home/.ssh/authorized_keys"
  legacy_slot_home_is_recognizable "$home"
  reconcile_slot_accounts "$homes"
  [ "$(cat "$home/.subyard-managed")" = test-vms-v1 ] || fail 'slot marker was not written exactly'
  slot_home_has_managed_marker "$home"
  chmod 0600 "$home/.subyard-managed"
  expect_failure reconcile_slot_accounts "$homes"
  chmod 0644 "$home/.subyard-managed"
  FIXTURE_UID=1001 expect_failure reconcile_slot_accounts "$homes"
  FIXTURE_GID=1001 expect_failure reconcile_slot_accounts "$homes"
  SLOT_ACCOUNT_HOME=/foreign expect_failure reconcile_slot_accounts "$homes"
  SLOT_ACCOUNT_SHELL=/bin/bash expect_failure reconcile_slot_accounts "$homes"
  printf 'foreign\n' > "$home/.subyard-managed"
  expect_failure reconcile_slot_accounts "$homes"
  # Even a recognizable legacy home must not override a present invalid marker.
  rm "$home/.subyard-managed"; ln -s "$TMP/config-output" "$home/.subyard-managed"
  expect_failure reconcile_slot_accounts "$homes"
  rm "$home/.subyard-managed"
  FIXTURE_SSH_GID=0 expect_failure reconcile_slot_accounts "$homes"
  FIXTURE_KEYS_GID=1002 expect_failure reconcile_slot_accounts "$homes"
  printf 'foreign key\n' > "$home/.ssh/authorized_keys"
  expect_failure reconcile_slot_accounts "$homes"
  : > "$home/.ssh/authorized_keys"; chmod 0660 "$home/.ssh/authorized_keys"
  expect_failure reconcile_slot_accounts "$homes"
  # A preexisting home without an account is subject to the same refusal.
  id() { case "$1" in -u) return 1;; -gn) printf '%s\n' "$fixture_primary";; *) return 64;; esac; }
  expect_failure reconcile_slot_accounts "$homes"
)

(
  mkdir "$TMP/firewall-bin"
  cat > "$TMP/firewall-bin/nft" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$NFT_CALLS"
[ "$*" != '-f -' ] || cat > "$NFT_RULES"
EOF
  chmod 0700 "$TMP/firewall-bin/nft"
  export PATH="$TMP/firewall-bin:$PATH" NFT_CALLS="$TMP/nft-calls" NFT_RULES="$TMP/nft-rules"
  systemctl() { printf '%s\n' "$*" >> "$TMP/firewall-systemctl"; }
  install_inner_firewall "$TMP/firewall" "$TMP/firewall.service"
  [ "$(stat -c '%a' "$TMP/firewall")" = 755 ] || fail 'firewall helper is not executable'
  for expected in '    iifname "e2e-vm-net-*" drop' \
    '    iifname "e2e-vm-net-*" oifname "e2e-vm-net-*" drop' \
    '    iifname "e2e-vm-net-*" ct direction reply ct state established,related accept' \
    '    iifname "e2e-vm-net-*" udp dport { 53, 67 } accept' \
    '    iifname "e2e-vm-net-*" tcp dport 53 accept'; do
    grep -Fxq "$expected" "$NFT_RULES" || fail "firewall lost rule: $expected"
  done
  [ "$(cat "$TMP/firewall-systemctl")" = $'daemon-reload\nenable --now subyard-test-vms-firewall.service' ] || fail 'firewall activation changed'
  "$TMP/firewall" remove
  expect_failure "$TMP/firewall" unknown
  [ "$(cat "$NFT_CALLS")" = $'delete table inet subyard_e2e\n-f -\ndelete table inet subyard_e2e' ] || fail 'firewall native requests changed'
)

# Execute the host sink installer with its existing output-path contract.
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$TMP/sink"
  account_fixture
  operator="$(command id -un)"
  id() { case "$1" in -u) printf '0\n';; -g) command id "$@";; *) return 64;; esac; }
  systemctl() { printf '%s\n' "$*" >> "$TMP/sink-calls"; }
  export -f id install systemctl
  export TMP ASSUME_YES=1 SUBYARD_USER="$operator"
  export SUBYARD_TEST_VMS_SINK_LIBEXEC_DIR="$TMP/sink/libexec"
  export SUBYARD_TEST_VMS_SINK_PATH="$TMP/sink/libexec/sink"
  export SUBYARD_TEST_VMS_SINK_SERVICE_PATH="$TMP/sink/host-sink.service"
  export SUBYARD_TEST_VMS_SINK_TIMER_PATH="$TMP/sink/host-sink.timer"
  export SUBYARD_TEST_VMS_SINK_ENGINE_SOURCE="$TMP/worker"
  bash "$ROOT/scripts/install-test-vms-host-sink.sh" --yes > "$TMP/sink-output"
  [ "$(cat "$TMP/sink-calls")" = $'daemon-reload\nenable --now host-sink.timer' ] || fail 'sink installer started a collection before producer rollout'
  cmp "$TMP/worker" "$SUBYARD_TEST_VMS_SINK_PATH" || fail 'sink lost selected engine'
  [ "$(stat -c '%a' "$SUBYARD_TEST_VMS_SINK_PATH")" = 755 ] \
    && [ "$(stat -c '%a' "$SUBYARD_TEST_VMS_SINK_SERVICE_PATH")" = 644 ] \
    && [ "$(stat -c '%a' "$SUBYARD_TEST_VMS_SINK_TIMER_PATH")" = 644 ] || fail 'sink installation modes changed'
  grep -Fxq "ExecStart=$SUBYARD_TEST_VMS_SINK_PATH _test-vms-host-sink sync" "$SUBYARD_TEST_VMS_SINK_SERVICE_PATH" || fail 'sink service lost engine entrypoint'
)

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
# Refuse unmanaged and non-container candidates without ever executing in them.
(
  incus() {
    case "$1" in
      config) printf '%s\n' "$FIXTURE_MANAGED" ;;
      list) printf '%s\n' "$FIXTURE_TYPE" ;;
      exec) : > "$TMP/seed-executed"; return 99 ;;
      *) return 64 ;;
    esac
  }
  export -f incus
  export TMP SUBYARD_E2E_VM=1 SUBYARD_E2E_LEGACY_FIXTURE=1
  FIXTURE_MANAGED=false FIXTURE_TYPE=CONTAINER
  export FIXTURE_MANAGED FIXTURE_TYPE
  expect_failure bash "$ROOT/dev/e2e/seed-test-vms-legacy-state.sh" fixture-project fixture-instance
  FIXTURE_MANAGED=true FIXTURE_TYPE=VIRTUAL-MACHINE
  expect_failure bash "$ROOT/dev/e2e/seed-test-vms-legacy-state.sh" fixture-project fixture-instance
  [ ! -e "$TMP/seed-executed" ] || fail 'legacy fixture executed in an unowned target'
)

E2E_VM_STATE_DIR="$TMP/legacy-test-vms-state"
mkdir -p "$E2E_VM_STATE_DIR"
chmod 2770 "$E2E_VM_STATE_DIR"
printf 'legacy\n' > "$E2E_VM_STATE_DIR/worker-key"
chmod 0660 "$E2E_VM_STATE_DIR/worker-key"
printf 'transient\n' > "$E2E_VM_STATE_DIR/.lease-state.123"
mkdir "$TMP/state-permission-bin"
cat > "$TMP/state-permission-bin/chmod" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *'/worker-key '*) rm -f -- "$TRANSIENT_LEASE_STATE" ;;
esac
exec "$REAL_CHMOD" "$@"
SH
chmod 0700 "$TMP/state-permission-bin/chmod"
real_chmod="$(command -v chmod)"
REAL_CHMOD="$real_chmod" \
TRANSIENT_LEASE_STATE="$E2E_VM_STATE_DIR/.lease-state.123" \
PATH="$TMP/state-permission-bin:$PATH" \
  reconcile_test_vm_state_directory
[ "$(stat -c '%a' "$E2E_VM_STATE_DIR")" = 700 ] \
  || fail "legacy test-vms state directory retained its setgid boundary"
[ "$(stat -c '%a' "$E2E_VM_STATE_DIR/worker-key")" = 600 ] \
  || fail "legacy test-vms state file retained group access"
[ ! -e "$E2E_VM_STATE_DIR/.lease-state.123" ] \
  || fail "test-vms state repair did not tolerate an atomic lease temp disappearing"

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
      # Keep writing beyond a pipe buffer to expose an early-closing consumer.
      printf 'fixture-unrelated-profile-device-%04d\n' {1..4096}
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
